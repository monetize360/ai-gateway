package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
	"golang.org/x/time/rate"
)

// Default ingest envelope (standard Kafka connection + InferenceUsage-shaped message).
var defaultBody = []byte(`{"connectionId":"b2c3d4e5-f6a7-4890-b123-456789abcdef","dataSourceId":"c3d4e5f6-a7b8-4901-c234-56789abcdef0","message":{"provider":"openai","model":"gpt-4o","prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"account":"k6-account","user":"k6-user"}}`)

func main() {
	url := flag.String("url", "http://127.0.0.1:8082/v1/ingest/kafka", "target URL")
	token := flag.String("token", envOr("MPILOT_ACCESS_TOKEN", envOr("AUTH_TOKEN", "")), "bearer token")
	targetRPS := flag.Int("rps", 20000, "target requests per second")
	duration := flag.Duration("duration", 60*time.Second, "test duration")
	concurrency := flag.Int("c", 8000, "worker goroutines")
	flag.Parse()

	if *token == "" {
		fmt.Fprintln(os.Stderr, "token required: -token or MPILOT_ACCESS_TOKEN")
		os.Exit(2)
	}

	auth := "Bearer " + *token
	burst := *targetRPS / 20
	if burst < 1 {
		burst = 1
	}
	limiter := rate.NewLimiter(rate.Limit(*targetRPS), burst)

	var (
		ok       atomic.Uint64
		fail     atomic.Uint64
		authFail atomic.Uint64
		badReq   atomic.Uint64
		latency  atomic.Uint64
	)

	client := &fasthttp.Client{
		MaxConnsPerHost:               *concurrency * 2,
		MaxIdleConnDuration:           30 * time.Second,
		ReadTimeout:                   15 * time.Second,
		WriteTimeout:                  15 * time.Second,
		MaxConnWaitTimeout:            5 * time.Second,
		DisableHeaderNamesNormalizing: true,
		NoDefaultUserAgentHeader:      true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(resp)

			req.Header.SetMethod(fasthttp.MethodPost)
			req.SetRequestURI(*url)
			req.Header.Set("Authorization", auth)
			req.Header.SetContentType("application/json")
			req.SetBody(defaultBody)

			for {
				if err := limiter.Wait(ctx); err != nil {
					return
				}
				start := time.Now()
				doErr := client.Do(req, resp)
				latency.Add(uint64(time.Since(start).Nanoseconds()))
				if doErr != nil {
					fail.Add(1)
					continue
				}
				switch resp.StatusCode() {
				case fasthttp.StatusOK:
					ok.Add(1)
				case fasthttp.StatusUnauthorized:
					authFail.Add(1)
				case fasthttp.StatusBadRequest:
					badReq.Add(1)
				default:
					fail.Add(1)
				}
				resp.Reset()
			}
		}()
	}

	fmt.Printf("loadgen target=%d rps duration=%s concurrency=%d url=%s\n", *targetRPS, *duration, *concurrency, *url)
	start := time.Now()
	var prev uint64
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			elapsed := time.Since(start).Seconds()
			totalOK := ok.Load()
			totalFail := fail.Load()
			totalAuth := authFail.Load()
			totalBad := badReq.Load()
			n := totalOK + totalFail + totalAuth + totalBad
			avgLatMs := 0.0
			if n > 0 {
				avgLatMs = float64(latency.Load()) / float64(n) / 1e6
			}
			avgRPS := 0.0
			if elapsed > 0 {
				avgRPS = float64(totalOK) / elapsed
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]any{
				"url":            *url,
				"duration_sec":   elapsed,
				"ok":             totalOK,
				"fail":           totalFail,
				"auth_fail":      totalAuth,
				"bad_request":    totalBad,
				"avg_rps":        avgRPS,
				"target_rps":     *targetRPS,
				"avg_latency_ms": avgLatMs,
				"hit_target":     avgRPS >= float64(*targetRPS)*0.95,
			})
			return
		case <-ticker.C:
			cur := ok.Load()
			fmt.Printf("ok=%d fail=%d authFail=%d badReq=%d instantRPS≈%d\n",
				cur, fail.Load(), authFail.Load(), badReq.Load(), cur-prev)
			prev = cur
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
