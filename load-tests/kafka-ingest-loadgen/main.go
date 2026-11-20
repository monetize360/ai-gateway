// Standalone Kafka producer loadgen.
//
// It does not start, call, or health-check the AI gateway. It writes the same
// usage record the POST /v1/ingest/usage path publishes:
//
//	topic inference-usage_<tenantID>
//	key   loadgen-txn-<n>, the same value as externalTransactionId
//	partitioner franz-go StickyKeyPartitioner (Kafka murmur2), same as kafkainject
//
// Each record gets its own key, so records spread across partitions by that id.
//
// After the produce window it waits until consumer group
// mpilot-usage-rating-<tenant> reports lag 0 on that topic. The rating worker
// commits that offset after it publishes the rated-transaction event.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	kgo "github.com/twmb/franz-go/pkg/kgo"
	"golang.org/x/time/rate"
)

const (
	defaultTenantID   = "b6c04e38-02f4-4c44-8e2e-44d38f489575"
	defaultOrgID      = "5c84e8ca-68c0-454f-83dd-7e995e7704e5"
	serviceID         = "819f8277-ca26-4239-ae38-ea55d655946f"
	billingAccount    = "koushik001"
	topicPrefix       = "inference-usage"
	ratingGroupPrefix = "mpilot-usage-rating"
)

func main() {
	profile := flag.String("profile", "5k", "rps preset: 5k or 10k (ignored when -rps is set)")
	rpsFlag := flag.Int("rps", 0, "target records per second; overrides -profile")
	duration := flag.Duration("duration", 60*time.Second, "test duration")
	brokers := flag.String("brokers", envOr("KAFKA_BOOTSTRAP_SERVERS", "localhost:29092"), "Kafka bootstrap servers")
	tenantID := flag.String("tenant-id", envOr("TENANT_ID", defaultTenantID), "tenant id appended to the usage topic")
	topicFlag := flag.String("topic", "", "topic override; default is inference-usage_<tenant-id>")
	orgID := flag.String("org-id", envOr("ORGANIZATION_ID", defaultOrgID), "organizationId written on each usage record")
	workers := flag.Int("c", 64, "producer goroutines")
	statsEvery := flag.Duration("stats-every", time.Second, "CPU and memory sample interval")
	statsOut := flag.String("stats-out", "kafka-ingest-loadgen-stats.csv", "CSV path for CPU and memory samples")
	watch := flag.String("watch", "", "comma-separated process name substrings to sample (for example the MPilot java process). The gateway is never started or called.")
	publishTimeout := flag.Duration("publish-timeout", 10*time.Minute, "after produce finishes, wait this long for the rating consumer group lag to reach 0")
	skipPublishWait := flag.Bool("skip-publish-wait", false, "do not wait for the rating consumer group after produce finishes")
	flag.Parse()

	targetRPS := *rpsFlag
	if targetRPS == 0 {
		switch strings.ToLower(*profile) {
		case "5k", "5000":
			targetRPS = 5000
		case "10k", "10000":
			targetRPS = 10000
		default:
			fmt.Fprintf(os.Stderr, "unknown profile %q (use 5k or 10k, or pass -rps)\n", *profile)
			os.Exit(2)
		}
	}
	if targetRPS < 1 {
		fmt.Fprintln(os.Stderr, "-rps must be positive")
		os.Exit(2)
	}
	if strings.TrimSpace(*tenantID) == "" {
		fmt.Fprintln(os.Stderr, "-tenant-id is required")
		os.Exit(2)
	}

	if cmd, hit := gatewayProcess(); hit {
		fmt.Fprintf(os.Stderr, "refusing to start: a gateway process is running (%s). Stop it first. This test only produces to Kafka.\n", cmd)
		os.Exit(3)
	}

	topic := strings.TrimSpace(*topicFlag)
	if topic == "" {
		topic = topicPrefix + "_" + strings.TrimSpace(*tenantID)
	}

	client, err := kgo.NewClient(
		kgo.SeedBrokers(splitBrokers(*brokers)...),
		kgo.ClientID("kafka-ingest-loadgen"),
		kgo.DefaultProduceTopic(topic),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerLinger(5*time.Millisecond),
		kgo.ProducerBatchMaxBytes(1<<20),
		kgo.MaxBufferedRecords(500_000),
		kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)),
		kgo.ProducerBatchCompression(kgo.Lz4Compression()),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kafka client: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := client.Ping(pingCtx); err != nil {
		pingCancel()
		fmt.Fprintf(os.Stderr, "kafka ping %s failed: %v\n", *brokers, err)
		os.Exit(1)
	}
	pingCancel()

	statsFile, err := os.Create(*statsOut)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stats file: %v\n", err)
		os.Exit(1)
	}
	defer statsFile.Close()
	fmt.Fprintln(statsFile, "ts,name,pid,cpu_percent,rss_mb")

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	statsCtx, statsCancel := context.WithCancel(context.Background())
	defer statsCancel()

	var samples statAgg
	var statsWG sync.WaitGroup
	statsWG.Add(1)
	go func() {
		defer statsWG.Done()
		sampleStats(statsCtx, *statsEvery, statsFile, *watch, &samples)
	}()

	burst := targetRPS / 20
	if burst < 1 {
		burst = 1
	}
	limiter := rate.NewLimiter(rate.Limit(targetRPS), burst)

	var (
		ok       atomic.Uint64
		fail     atomic.Uint64
		latency  atomic.Uint64
		seq      atomic.Uint64
		firstErr atomic.Pointer[produceErrHolder]
	)
	org := strings.TrimSpace(*orgID)

	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if err := limiter.Wait(ctx); err != nil {
					return
				}
				txnID := fmt.Sprintf("loadgen-txn-%d", seq.Add(1))
				payload := usagePayload(org, txnID)
				start := time.Now()
				client.Produce(context.Background(), &kgo.Record{
					Topic: topic,
					Key:   []byte(txnID),
					Value: payload,
				}, func(_ *kgo.Record, produceErr error) {
					latency.Add(uint64(time.Since(start).Nanoseconds()))
					if produceErr != nil {
						fail.Add(1)
						firstErr.CompareAndSwap(nil, &produceErrHolder{err: produceErr})
						return
					}
					ok.Add(1)
				})
			}
		}()
	}

	fmt.Printf("kafka-loadgen topic=%s key=loadgen-txn-<n> rps=%d duration=%s brokers=%s workers=%d\n",
		topic, targetRPS, *duration, *brokers, *workers)
	fmt.Println("gateway is not used: no startup, no health check, no HTTP call")

	start := time.Now()
	var prev uint64
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			flushCtx, flushCancel := context.WithTimeout(context.Background(), 30*time.Second)
			flushErr := client.Flush(flushCtx)
			flushCancel()

			produceDone := time.Now()
			elapsed := produceDone.Sub(start).Seconds()
			totalOK := ok.Load()
			totalFail := fail.Load()
			n := totalOK + totalFail
			avgLatMs := 0.0
			if n > 0 {
				avgLatMs = float64(latency.Load()) / float64(n) / 1e6
			}
			avgRPS := 0.0
			if elapsed > 0 {
				avgRPS = float64(totalOK) / elapsed
			}

			ratingGroup := ratingGroupPrefix + "-" + strings.TrimSpace(*tenantID)
			publish := publishResult{Status: "skipped"}
			switch {
			case *skipPublishWait:
				fmt.Println("publish wait skipped")
			case totalOK == 0:
				fmt.Println("publish wait skipped: no records produced")
			default:
				fmt.Printf("waiting for %s lag on %s to reach 0 (timeout %s)\n", ratingGroup, topic, publishTimeout.String())
				publish = waitForRatingPublish(client, ratingGroup, topic, produceDone, *publishTimeout)
			}

			statsCancel()
			statsWG.Wait()

			summary := map[string]any{
				"topic":           topic,
				"key":             "externalTransactionId",
				"partitioner":     "sticky-key",
				"tenant_id":       *tenantID,
				"brokers":         *brokers,
				"rating_group":    ratingGroup,
				"duration_sec":    elapsed,
				"produce_done_at": produceDone.UTC().Format(time.RFC3339),
				"ok":              totalOK,
				"fail":            totalFail,
				"avg_rps":         avgRPS,
				"target_rps":      targetRPS,
				"avg_latency_ms":  avgLatMs,
				"hit_target":      avgRPS >= float64(targetRPS)*0.95,
				"publish_status":  publish.Status,
				"total_sec":       time.Since(start).Seconds(),
				"stats_csv":       *statsOut,
				"cpu_mem":         samples.snapshot(),
			}
			if publish.Status != "skipped" {
				summary["publish_elapsed_sec"] = publish.Elapsed.Seconds()
				summary["publish_lag"] = publish.Lag
			}
			if !publish.DoneAt.IsZero() {
				summary["publish_done_at"] = publish.DoneAt.UTC().Format(time.RFC3339)
			}
			if publish.Err != nil {
				summary["publish_error"] = publish.Err.Error()
			}
			if flushErr != nil {
				summary["flush_error"] = flushErr.Error()
			}
			if held := firstErr.Load(); held != nil {
				summary["first_produce_error"] = held.err.Error()
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(summary)
			if flushErr != nil || totalFail > 0 && totalOK == 0 {
				os.Exit(1)
			}
			if publish.Status != "caught_up" && publish.Status != "skipped" {
				os.Exit(4)
			}
			return
		case <-ticker.C:
			cur := ok.Load()
			fmt.Printf("ok=%d fail=%d instantRPS≈%d\n", cur, fail.Load(), cur-prev)
			prev = cur
		}
	}
}

// produceErrHolder wraps an error so it can be stored in an atomic.Pointer.
type produceErrHolder struct {
	err error
}

type publishResult struct {
	Status  string
	Lag     int64
	Err     error
	DoneAt  time.Time
	Elapsed time.Duration
}

// waitForRatingPublish polls the rating consumer group until every partition of
// topic has lag 0. That commit happens after the rating worker publishes the
// rated-transaction event, so lag 0 is the publish-event step finishing.
func waitForRatingPublish(client *kgo.Client, group, topic string, produceDone time.Time, timeout time.Duration) publishResult {
	adm := kadm.NewClient(client)
	deadline := time.Now().Add(timeout)
	var last publishResult
	lastPrintedAt := time.Time{}
	for {
		pollCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		lags, err := adm.Lag(pollCtx, group)
		cancel()

		now := time.Now()
		last.Elapsed = now.Sub(produceDone)
		if err != nil {
			last.Err = err
			last.Status = "error"
			if errors.Is(err, kerr.GroupIDNotFound) {
				last.Status = "group_missing"
			}
		} else {
			lag, status, lagErr := ratingTopicLag(lags, group, topic)
			last.Lag = lag
			last.Status = status
			last.Err = lagErr
			if status == "caught_up" {
				last.DoneAt = now
				fmt.Printf("publish wait group=%s status=caught_up lag=0 elapsed=%.1fs\n", group, last.Elapsed.Seconds())
				return last
			}
		}
		// One progress line every 10s. The full stream is not kept.
		if lastPrintedAt.IsZero() || now.Sub(lastPrintedAt) >= 10*time.Second {
			fmt.Printf("publish wait group=%s status=%s lag=%d elapsed=%.1fs\n", group, last.Status, last.Lag, last.Elapsed.Seconds())
			lastPrintedAt = now
		}
		if !now.Before(deadline) {
			if last.Status == "waiting" {
				last.Status = "timeout"
			}
			return last
		}
		sleep := time.Second
		if remain := time.Until(deadline); remain < sleep {
			sleep = remain
		}
		if sleep > 0 {
			time.Sleep(sleep)
		}
	}
}

func ratingTopicLag(lags kadm.DescribedGroupLags, group, topic string) (int64, string, error) {
	described, ok := lags[group]
	if !ok {
		return 0, "group_missing", fmt.Errorf("group %s was not returned", group)
	}
	if described.DescribeErr != nil {
		if errors.Is(described.DescribeErr, kerr.GroupIDNotFound) || described.State == "Dead" {
			return 0, "group_missing", described.DescribeErr
		}
		return 0, "error", described.DescribeErr
	}
	if described.State == "Dead" {
		return 0, "group_missing", fmt.Errorf("group %s is Dead", group)
	}
	if described.FetchErr != nil {
		return 0, "error", described.FetchErr
	}
	partitions := described.Lag[topic]
	if len(partitions) == 0 {
		return 0, "topic_missing", fmt.Errorf("topic %s has no lag rows (group state %s)", topic, described.State)
	}
	var sum int64
	for _, part := range partitions {
		if part.Err != nil {
			return sum, "error", fmt.Errorf("partition %d: %w", part.Partition, part.Err)
		}
		if part.Lag < 0 {
			return sum, "error", fmt.Errorf("partition %d lag unavailable", part.Partition)
		}
		sum += part.Lag
	}
	if sum == 0 {
		return 0, "caught_up", nil
	}
	return sum, "waiting", nil
}

func usagePayload(orgID, txnID string) []byte {
	msg := map[string]any{
		"organizationId":        orgID,
		"externalTransactionId": txnID,
		"serviceId":             serviceID,
		"eventTimestamp":        time.Now().UTC().Format(time.RFC3339Nano),
		"billingAccountRef":     billingAccount,
		"dimensions": map[string]string{
			"model":              "moonshotai/Kimi-VL-A3B-Thinking",
			"ttft_ms":            "882",
			"provider":           "DigiPowerX",
			"input_token":        "83",
			"output_token":       "901",
			"api_latency_ms":     "3375",
			"virtual_key_id":     "eb465bf1-88b8-4a27-b67e-e3ef5fbf78a7",
			"cached_input_token": "32",
		},
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

type procSample struct {
	name    string
	pid     int
	cpuSum  float64
	cpuMax  float64
	rssSum  float64
	rssMax  float64
	samples int
}

type statAgg struct {
	mu    sync.Mutex
	procs map[string]*procSample
}

func (s *statAgg) add(name string, pid int, cpu, rssMB float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.procs == nil {
		s.procs = map[string]*procSample{}
	}
	key := name + "#" + strconv.Itoa(pid)
	row := s.procs[key]
	if row == nil {
		row = &procSample{name: name, pid: pid}
		s.procs[key] = row
	}
	row.cpuSum += cpu
	row.rssSum += rssMB
	if cpu > row.cpuMax {
		row.cpuMax = cpu
	}
	if rssMB > row.rssMax {
		row.rssMax = rssMB
	}
	row.samples++
}

func (s *statAgg) snapshot() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(s.procs))
	for _, row := range s.procs {
		avgCPU, avgRSS := 0.0, 0.0
		if row.samples > 0 {
			avgCPU = row.cpuSum / float64(row.samples)
			avgRSS = row.rssSum / float64(row.samples)
		}
		out = append(out, map[string]any{
			"name":        row.name,
			"pid":         row.pid,
			"samples":     row.samples,
			"avg_cpu_pct": avgCPU,
			"max_cpu_pct": row.cpuMax,
			"avg_rss_mb":  avgRSS,
			"max_rss_mb":  row.rssMax,
		})
	}
	return out
}

func sampleStats(ctx context.Context, every time.Duration, out *os.File, watch string, agg *statAgg) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	needles := splitBrokers(watch)
	self := os.Getpid()
	for {
		select {
		case <-ctx.Done():
			writeSample(out, agg, self, "kafka-ingest-loadgen")
			for _, needle := range needles {
				sampleMatching(out, agg, needle)
			}
			return
		case <-ticker.C:
			writeSample(out, agg, self, "kafka-ingest-loadgen")
			for _, needle := range needles {
				sampleMatching(out, agg, needle)
			}
		}
	}
}

func writeSample(out *os.File, agg *statAgg, pid int, name string) {
	cpu, rssMB, ok := readProc(pid)
	if !ok {
		return
	}
	fmt.Fprintf(out, "%s,%s,%d,%.2f,%.2f\n", time.Now().UTC().Format(time.RFC3339), name, pid, cpu, rssMB)
	agg.add(name, pid, cpu, rssMB)
}

func sampleMatching(out *os.File, agg *statAgg, needle string) {
	if needle == "" {
		return
	}
	cmd := exec.Command("ps", "-ax", "-o", "pid=,%cpu=,rss=,command=")
	raw, err := cmd.Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		command := strings.Join(fields[3:], " ")
		if !strings.Contains(command, needle) || looksLikeGateway(command) {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == os.Getpid() {
			continue
		}
		cpu, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		rssKB, err := strconv.ParseFloat(fields[2], 64)
		if err != nil {
			continue
		}
		rssMB := rssKB / 1024
		fmt.Fprintf(out, "%s,%s,%d,%.2f,%.2f\n", time.Now().UTC().Format(time.RFC3339), needle, pid, cpu, rssMB)
		agg.add(needle, pid, cpu, rssMB)
	}
}

func readProc(pid int) (cpu, rssMB float64, ok bool) {
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "%cpu=", "-o", "rss=")
	raw, err := cmd.Output()
	if err != nil {
		return 0, 0, false
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 2 {
		return 0, 0, false
	}
	cpu, err = strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, 0, false
	}
	rssKB, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return 0, 0, false
	}
	return cpu, rssKB / 1024, true
}

func gatewayProcess() (string, bool) {
	cmd := exec.Command("ps", "-ax", "-o", "pid=,command=")
	raw, err := cmd.Output()
	if err != nil {
		return "", false
	}
	self := strconv.Itoa(os.Getpid())
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, self+" ") {
			continue
		}
		if looksLikeGateway(line) {
			return line, true
		}
	}
	return "", false
}

func looksLikeGateway(command string) bool {
	lower := strings.ToLower(command)
	return strings.Contains(lower, "bifrost-http") ||
		strings.Contains(lower, "start-dev.sh") ||
		strings.Contains(lower, "/cmd/bifrost") ||
		strings.Contains(lower, "bifrost serve")
}

func splitBrokers(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
