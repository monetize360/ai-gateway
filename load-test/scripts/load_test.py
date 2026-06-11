#!/usr/bin/env python3
"""
Load test: fires LLM inference calls through dev Bifrost and measures how long
Kafka (observability connector) and Postgres (logs_store) take to catch up.

Usage:
    python scripts/load_test.py --total 10000000 --concurrency 200
"""

from __future__ import annotations

import argparse
import asyncio
import json
import math
import subprocess
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Callable, Optional

try:
    import aiohttp
except ImportError:
    print("aiohttp is required. Install with: pip install aiohttp")
    sys.exit(1)


TOTAL_REQUESTS_DEFAULT = 5_000
CONCURRENCY_DEFAULT = 50
CHUNK_SIZE_DEFAULT = 10_000
BIFROST_URL_DEFAULT = "http://localhost:8080"
KAFKA_UI_URL_DEFAULT = "http://localhost:8090"
TOPIC_NAME_DEFAULT = "bifrost-traces"
POSTGRES_PORT_DEFAULT = "5433"
POSTGRES_USER_DEFAULT = "bifrost"
POSTGRES_PASSWORD_DEFAULT = "bifrost_password"
POSTGRES_DB_DEFAULT = "bifrost"
FLUSH_POLL_INTERVAL_DEFAULT = 2.0
FLUSH_TIMEOUT_DEFAULT = 14_400.0  # 4 hours
FLUSH_STABLE_POLLS_DEFAULT = 3

SAMPLE_PROMPTS = [
    "Explain the theory of relativity in simple terms.",
    "Write a haiku about the ocean.",
    "What is the capital of France?",
    "Summarise the plot of Hamlet in three sentences.",
    "Give me a Python function to compute Fibonacci numbers.",
    "What are the main causes of climate change?",
    "Describe the water cycle.",
    "How does a neural network learn?",
    "What is Kafka used for in distributed systems?",
    "Explain the difference between TCP and UDP.",
]


@dataclass
class OnlineLatencyStats:
    count: int = 0
    success: int = 0
    failure: int = 0
    sum_ms: float = 0.0
    min_ms: float = math.inf
    max_ms: float = 0.0
    # Reservoir for approximate percentiles without storing every sample.
    _reservoir: list[float] = field(default_factory=list)
    _reservoir_max: int = 50_000

    def record(self, success: bool, latency_ms: float) -> None:
        self.count += 1
        if success:
            self.success += 1
        else:
            self.failure += 1
        self.sum_ms += latency_ms
        self.min_ms = min(self.min_ms, latency_ms)
        self.max_ms = max(self.max_ms, latency_ms)
        if len(self._reservoir) < self._reservoir_max:
            self._reservoir.append(latency_ms)
        else:
            # Reservoir sampling: replace random element occasionally.
            idx = self.count % self._reservoir_max
            self._reservoir[idx] = latency_ms

    def percentile(self, p: float) -> float:
        if not self._reservoir:
            return 0.0
        sorted_lat = sorted(self._reservoir)
        idx = int(len(sorted_lat) * p / 100)
        return sorted_lat[min(idx, len(sorted_lat) - 1)]

    def print_summary(self, elapsed_seconds: float) -> None:
        rps = self.success / elapsed_seconds if elapsed_seconds > 0 else 0
        avg_lat = self.sum_ms / self.count if self.count else 0
        print("\n" + "=" * 60)
        print("  Load Test Summary")
        print("=" * 60)
        print(f"  Total requests  : {self.count}")
        print(f"  Successful      : {self.success}")
        print(f"  Failed          : {self.failure}")
        print(f"  Elapsed time    : {elapsed_seconds:.2f}s")
        print(f"  Throughput      : {rps:.1f} req/s")
        print(f"  Latency avg     : {avg_lat:.1f} ms")
        print(f"  Latency p50     : {self.percentile(50):.1f} ms (sampled)")
        print(f"  Latency p95     : {self.percentile(95):.1f} ms (sampled)")
        print(f"  Latency p99     : {self.percentile(99):.1f} ms (sampled)")
        print("=" * 60)


@dataclass
class FlushMetrics:
    target: int
    kafka_messages: Optional[int] = None
    postgres_logs: Optional[int] = None
    kafka_flush_seconds: Optional[float] = None
    postgres_flush_seconds: Optional[float] = None
    kafka_timed_out: bool = False
    postgres_timed_out: bool = False
    kafka_skipped: bool = False
    postgres_skipped: bool = False


def run_cmd(cmd: list[str], timeout: float = 30.0) -> tuple[int, str, str]:
    try:
        proc = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=timeout,
            check=False,
        )
        return proc.returncode, proc.stdout.strip(), proc.stderr.strip()
    except subprocess.TimeoutExpired:
        return 124, "", "timeout"
    except FileNotFoundError as exc:
        return 127, "", str(exc)


def docker_container_running(name: str) -> bool:
    code, out, _ = run_cmd(
        ["docker", "ps", "--format", "{{.Names}}"],
        timeout=10,
    )
    if code != 0:
        return False
    return any(line.strip() == name for line in out.splitlines())


def resolve_postgres_container(explicit: str) -> Optional[str]:
    if explicit:
        return explicit if docker_container_running(explicit) else None
    for name in ("bifrost-postgres", "bifrost-postgres-fw"):
        if docker_container_running(name):
            return name
    return None


def get_kafka_topic_messages(kafka_container: str, topic: str) -> Optional[int]:
    code, out, _ = run_cmd(
        [
            "docker",
            "exec",
            kafka_container,
            "/opt/kafka/bin/kafka-get-offsets.sh",
            "--bootstrap-server",
            "localhost:9092",
            "--topic",
            topic,
        ],
        timeout=60,
    )
    if code != 0 or not out:
        return None
    total = 0
    for line in out.splitlines():
        # bifrost-traces:0:12345
        parts = line.strip().split(":")
        if len(parts) >= 3 and parts[-2].isdigit():
            try:
                total += int(parts[-1])
            except ValueError:
                continue
    return total


def _postgres_query(container: str, user: str, password: str, db: str, sql: str) -> Optional[int]:
    code, out, _ = run_cmd(
        [
            "docker",
            "exec",
            "-e",
            f"PGPASSWORD={password}",
            container,
            "psql",
            "-U",
            user,
            "-d",
            db,
            "-tAc",
            sql,
        ],
        timeout=300,
    )
    if code != 0 or not out:
        return None
    try:
        return int(out.strip())
    except ValueError:
        return None


def get_postgres_log_count_estimate(
    container: str,
    user: str,
    password: str,
    db: str,
) -> Optional[int]:
    # Fast estimate for large tables (updated by autovacuum/analyze).
    return _postgres_query(
        container,
        user,
        password,
        db,
        "SELECT COALESCE(n_live_tup, 0)::bigint FROM pg_stat_user_tables WHERE relname = 'logs';",
    )


def get_postgres_log_count_exact(
    container: str,
    user: str,
    password: str,
    db: str,
) -> Optional[int]:
    return _postgres_query(
        container,
        user,
        password,
        db,
        "SELECT COUNT(*) FROM finops_logs;",
    )


def get_kafka_ui_message_estimate(kafka_ui_url: str, topic: str) -> Optional[int]:
    try:
        req = urllib.request.Request(
            f"{kafka_ui_url}/api/clusters/local/topics/{topic}",
            headers={"Accept": "application/json"},
        )
        with urllib.request.urlopen(req, timeout=15) as resp:
            topic_data = json.loads(resp.read().decode())
        partitions = topic_data.get("partitions", [])
        return sum(
            p.get("offsetMax", 0) - p.get("offsetMin", 0) for p in partitions
        )
    except (urllib.error.URLError, json.JSONDecodeError, KeyError, ValueError):
        return None


def wait_for_sink(
    name: str,
    target: int,
    poll_fn: Callable[[], Optional[int]],
    t0: float,
    poll_interval: float,
    timeout: float,
    stable_polls: int,
) -> tuple[Optional[int], Optional[float], bool]:
    """
    Returns (final_count, seconds_since_t0, timed_out).
    'Flush complete' when count >= target and unchanged for stable_polls consecutive polls.
    """
    last_count: Optional[int] = None
    stable = 0
    deadline = time.monotonic() + timeout

    print(f"\n  Measuring {name} flush (target >= {target:,} records)…")
    while time.monotonic() < deadline:
        count = poll_fn()
        if count is None:
            time.sleep(poll_interval)
            continue

        elapsed = time.monotonic() - t0
        if count >= target:
            if last_count is not None and count == last_count:
                stable += 1
            else:
                stable = 0
            if stable >= stable_polls:
                print(f"  {name}: {count:,} records — flush complete in {elapsed:.2f}s")
                return count, elapsed, False
        else:
            stable = 0

        if int(elapsed) % 30 == 0 and int(elapsed) > 0:
            print(f"  {name}: {count:,} / {target:,} ({100.0 * count / target:.1f}%) @ {elapsed:.0f}s")

        last_count = count
        time.sleep(poll_interval)

    final = poll_fn()
    elapsed = time.monotonic() - t0
    print(f"  {name}: timed out after {elapsed:.0f}s (last count={final})")
    return final, elapsed if final is not None else None, True


async def send_request(
    session: aiohttp.ClientSession,
    bifrost_url: str,
    request_id: int,
    stats: OnlineLatencyStats,
    semaphore: asyncio.Semaphore,
) -> None:
    prompt = SAMPLE_PROMPTS[request_id % len(SAMPLE_PROMPTS)]
    payload = {
        "model": "fake-llm/gpt-4o-mini",
        "messages": [{"role": "user", "content": prompt}],
    }

    start = time.monotonic()
    success = False
    try:
        async with semaphore:
            async with session.post(
                f"{bifrost_url}/v1/chat/completions",
                json=payload,
                timeout=aiohttp.ClientTimeout(total=60),
            ) as resp:
                body = await resp.json()
                success = resp.status == 200 and "choices" in body
    except Exception:
        pass

    stats.record(success, (time.monotonic() - start) * 1000)


async def run_requests(
    total: int,
    concurrency: int,
    bifrost_url: str,
    chunk_size: int,
    progress_interval: int,
) -> tuple[OnlineLatencyStats, float]:
    stats = OnlineLatencyStats()
    semaphore = asyncio.Semaphore(concurrency)
    progress_counter = 0

    connector = aiohttp.TCPConnector(limit=concurrency + 20, ttl_dns_cache=300)
    async with aiohttp.ClientSession(connector=connector) as session:
        start_time = time.monotonic()
        for chunk_start in range(0, total, chunk_size):
            chunk_end = min(chunk_start + chunk_size, total)
            tasks = [
                send_request(session, bifrost_url, i, stats, semaphore)
                for i in range(chunk_start, chunk_end)
            ]
            await asyncio.gather(*tasks)
            progress_counter = chunk_end
            if progress_counter % progress_interval == 0 or progress_counter == total:
                pct = 100.0 * progress_counter / total
                elapsed = time.monotonic() - start_time
                rps = progress_counter / elapsed if elapsed > 0 else 0
                print(
                    f"  [{progress_counter:>12,}/{total:,}] {pct:5.1f}%  "
                    f"success={stats.success:,}  fail={stats.failure:,}  "
                    f"throughput={rps:,.0f} req/s"
                )
        elapsed = time.monotonic() - start_time
    return stats, elapsed


def measure_flush(
    stats: OnlineLatencyStats,
    *,
    kafka_container: str,
    kafka_topic: str,
    kafka_ui_url: str,
    postgres_container: Optional[str],
    postgres_user: str,
    postgres_password: str,
    postgres_db: str,
    poll_interval: float,
    flush_timeout: float,
    stable_polls: int,
) -> FlushMetrics:
    target = stats.success
    metrics = FlushMetrics(target=target)
    if target == 0:
        print("\n  No successful requests — skipping flush measurement.")
        return metrics

    t0 = time.monotonic()

    if docker_container_running(kafka_container):
        count, secs, timed_out = wait_for_sink(
            "Kafka",
            target,
            lambda: get_kafka_topic_messages(kafka_container, kafka_topic),
            t0,
            poll_interval,
            flush_timeout,
            stable_polls,
        )
        metrics.kafka_messages = count
        metrics.kafka_flush_seconds = secs
        metrics.kafka_timed_out = timed_out
        if count is None:
            ui_count = get_kafka_ui_message_estimate(kafka_ui_url, kafka_topic)
            if ui_count is not None:
                metrics.kafka_messages = ui_count
                print(f"  Kafka UI estimate: {ui_count:,} messages")
    else:
        metrics.kafka_skipped = True
        print(f"\n  Kafka container '{kafka_container}' not running — skipping Kafka flush metrics.")

    if postgres_container:
        pg_args = (postgres_container, postgres_user, postgres_password, postgres_db)
        count, secs, timed_out = wait_for_sink(
            "Postgres (logs, estimate)",
            target,
            lambda: get_postgres_log_count_estimate(*pg_args),
            t0,
            poll_interval,
            flush_timeout,
            stable_polls,
        )
        exact = get_postgres_log_count_exact(*pg_args)
        if exact is not None:
            print(f"  Postgres exact COUNT(*): {exact:,}")
            count = exact
        metrics.postgres_logs = count
        metrics.postgres_flush_seconds = secs
        metrics.postgres_timed_out = timed_out
    else:
        metrics.postgres_skipped = True
        print("\n  Postgres container not found — skipping Postgres flush metrics.")

    return metrics


def print_flush_report(metrics: FlushMetrics) -> None:
    print("\n" + "=" * 60)
    print("  Sink Flush Metrics")
    print("=" * 60)
    print(f"  Target (successful requests) : {metrics.target:,}")
    if metrics.kafka_skipped:
        print("  Kafka                        : skipped")
    else:
        print(f"  Kafka messages               : {metrics.kafka_messages}")
        if metrics.kafka_flush_seconds is not None:
            print(f"  Kafka flush time             : {metrics.kafka_flush_seconds:.2f}s")
        if metrics.kafka_timed_out:
            print("  Kafka                        : TIMED OUT before stable")
        if metrics.kafka_messages is not None and metrics.kafka_messages < metrics.target:
            print(
                f"  Kafka gap                    : {metrics.target - metrics.kafka_messages:,} "
                "(buffer drops or delivery errors)"
            )
    if metrics.postgres_skipped:
        print("  Postgres logs                : skipped")
    else:
        print(f"  Postgres log rows            : {metrics.postgres_logs}")
        if metrics.postgres_flush_seconds is not None:
            print(f"  Postgres flush time          : {metrics.postgres_flush_seconds:.2f}s")
        if metrics.postgres_timed_out:
            print("  Postgres                     : TIMED OUT before stable")
        if metrics.postgres_logs is not None and metrics.postgres_logs < metrics.target:
            print(
                f"  Postgres gap                 : {metrics.target - metrics.postgres_logs:,} "
                "(async logging backlog or failures)"
            )
    print("=" * 60)


def write_metrics_json(path: str, payload: dict) -> None:
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(payload, fh, indent=2)
    print(f"\n  Metrics written to: {path}")


async def run_load_test(args: argparse.Namespace) -> bool:
    print("=" * 60)
    print("  Bifrost Load Test")
    print("=" * 60)
    print(f"  Target          : {args.bifrost_url}")
    print(f"  Total requests  : {args.total:,}")
    print(f"  Concurrency     : {args.concurrency}")
    print(f"  Chunk size      : {args.chunk_size:,}")
    print(f"  Kafka topic     : {args.kafka_topic}")
    print(f"  Measure flush   : {args.measure_flush}")
    print("=" * 60 + "\n")

    progress_interval = args.progress_interval
    if progress_interval <= 0:
        progress_interval = max(args.total // 100, 1)

    stats, elapsed = await run_requests(
        args.total,
        args.concurrency,
        args.bifrost_url,
        args.chunk_size,
        progress_interval,
    )
    stats.print_summary(elapsed)

    flush_metrics = FlushMetrics(target=stats.success)
    if args.measure_flush:
        pg_container = resolve_postgres_container(args.postgres_container)
        flush_metrics = measure_flush(
            stats,
            kafka_container=args.kafka_container,
            kafka_topic=args.kafka_topic,
            kafka_ui_url=args.kafka_ui_url,
            postgres_container=pg_container,
            postgres_user=args.postgres_user,
            postgres_password=args.postgres_password,
            postgres_db=args.postgres_db,
            poll_interval=args.flush_poll_interval,
            flush_timeout=args.flush_timeout,
            stable_polls=args.flush_stable_polls,
        )
        print_flush_report(flush_metrics)

    if args.metrics_out:
        payload = {
            "timestamp": datetime.now(timezone.utc).isoformat(),
            "requests": {
                "total": stats.count,
                "success": stats.success,
                "failure": stats.failure,
                "elapsed_seconds": elapsed,
                "throughput_rps": stats.success / elapsed if elapsed > 0 else 0,
            },
            "flush": {
                "target": flush_metrics.target,
                "kafka_messages": flush_metrics.kafka_messages,
                "kafka_flush_seconds": flush_metrics.kafka_flush_seconds,
                "kafka_timed_out": flush_metrics.kafka_timed_out,
                "postgres_logs": flush_metrics.postgres_logs,
                "postgres_flush_seconds": flush_metrics.postgres_flush_seconds,
                "postgres_timed_out": flush_metrics.postgres_timed_out,
            },
        }
        write_metrics_json(args.metrics_out, payload)

    print(f"\n  Bifrost UI : {args.bifrost_url}")
    print(f"  Kafka UI   : {args.kafka_ui_url}")
    return stats.failure == 0


def main() -> None:
    parser = argparse.ArgumentParser(description="Bifrost load test with sink flush metrics")
    parser.add_argument("--total", type=int, default=TOTAL_REQUESTS_DEFAULT)
    parser.add_argument("--concurrency", type=int, default=CONCURRENCY_DEFAULT)
    parser.add_argument("--chunk-size", type=int, default=CHUNK_SIZE_DEFAULT)
    parser.add_argument("--progress-interval", type=int, default=0)
    parser.add_argument("--bifrost-url", default=BIFROST_URL_DEFAULT)
    parser.add_argument("--kafka-ui-url", default=KAFKA_UI_URL_DEFAULT)
    parser.add_argument("--kafka-topic", default=TOPIC_NAME_DEFAULT)
    parser.add_argument("--kafka-container", default="kafka")
    parser.add_argument("--postgres-container", default="")
    parser.add_argument("--postgres-user", default=POSTGRES_USER_DEFAULT)
    parser.add_argument("--postgres-password", default=POSTGRES_PASSWORD_DEFAULT)
    parser.add_argument("--postgres-db", default=POSTGRES_DB_DEFAULT)
    parser.add_argument("--measure-flush", action=argparse.BooleanOptionalAction, default=True)
    parser.add_argument("--flush-poll-interval", type=float, default=FLUSH_POLL_INTERVAL_DEFAULT)
    parser.add_argument("--flush-timeout", type=float, default=FLUSH_TIMEOUT_DEFAULT)
    parser.add_argument("--flush-stable-polls", type=int, default=FLUSH_STABLE_POLLS_DEFAULT)
    parser.add_argument(
        "--metrics-out",
        default="",
        help="Write JSON metrics to this path (default: load-test/results/metrics-<ts>.json)",
    )
    args = parser.parse_args()

    if not args.metrics_out:
        ts = datetime.now().strftime("%Y%m%d-%H%M%S")
        args.metrics_out = f"results/metrics-{ts}.json"

    ok = asyncio.run(run_load_test(args))
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
