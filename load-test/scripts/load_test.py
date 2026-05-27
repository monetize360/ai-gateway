#!/usr/bin/env python3
"""
Load test: fires dummy LLM inference calls through the local dev Bifrost gateway
and then checks whether the Kafka observability connector published trace records.

Usage:
    python scripts/load_test.py [--total 5000] [--concurrency 50]
                                [--bifrost-url http://localhost:8080]
                                [--kafka-ui-url http://localhost:8090]
                                [--kafka-topic bifrost-traces]
"""

import argparse
import asyncio
import json
import sys
import time
import urllib.request
import urllib.error
from dataclasses import dataclass, field


try:
    import aiohttp
except ImportError:
    print("aiohttp is required. Install with: pip install aiohttp")
    sys.exit(1)


# ─────────────────────────────────────────────────────────────────────────────
# Configuration
# ─────────────────────────────────────────────────────────────────────────────

TOTAL_REQUESTS_DEFAULT = 5000
CONCURRENCY_DEFAULT = 50
BIFROST_URL_DEFAULT = "http://localhost:8080"
KAFKA_UI_URL_DEFAULT = "http://localhost:8090"
TOPIC_NAME_DEFAULT = "bifrost-traces"

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


# ─────────────────────────────────────────────────────────────────────────────
# Stats collector
# ─────────────────────────────────────────────────────────────────────────────

@dataclass
class Stats:
    total: int = 0
    success: int = 0
    failure: int = 0
    latencies: list = field(default_factory=list)

    def record(self, success: bool, latency_ms: float):
        self.total += 1
        if success:
            self.success += 1
        else:
            self.failure += 1
        self.latencies.append(latency_ms)

    def percentile(self, p: float) -> float:
        if not self.latencies:
            return 0.0
        sorted_lat = sorted(self.latencies)
        idx = int(len(sorted_lat) * p / 100)
        return sorted_lat[min(idx, len(sorted_lat) - 1)]

    def print_summary(self, elapsed_seconds: float):
        rps = self.success / elapsed_seconds if elapsed_seconds > 0 else 0
        avg_lat = sum(self.latencies) / len(self.latencies) if self.latencies else 0
        print("\n" + "=" * 60)
        print("  Load Test Summary")
        print("=" * 60)
        print(f"  Total requests  : {self.total}")
        print(f"  Successful      : {self.success}")
        print(f"  Failed          : {self.failure}")
        print(f"  Elapsed time    : {elapsed_seconds:.2f}s")
        print(f"  Throughput      : {rps:.1f} req/s")
        print(f"  Latency avg     : {avg_lat:.1f} ms")
        print(f"  Latency p50     : {self.percentile(50):.1f} ms")
        print(f"  Latency p95     : {self.percentile(95):.1f} ms")
        print(f"  Latency p99     : {self.percentile(99):.1f} ms")
        print("=" * 60)


# ─────────────────────────────────────────────────────────────────────────────
# Request worker
# ─────────────────────────────────────────────────────────────────────────────

async def send_request(
    session: "aiohttp.ClientSession",
    bifrost_url: str,
    request_id: int,
    stats: Stats,
    semaphore: asyncio.Semaphore,
    progress_counter: list,
    total: int,
):
    prompt = SAMPLE_PROMPTS[request_id % len(SAMPLE_PROMPTS)]
    payload = {
        "model": "fake-llm/gpt-4o-mini",
        "messages": [
            {"role": "user", "content": prompt},
        ],
    }

    start = time.monotonic()
    success = False
    try:
        async with semaphore:
            async with session.post(
                f"{bifrost_url}/v1/chat/completions",
                json=payload,
                timeout=aiohttp.ClientTimeout(total=30),
            ) as resp:
                body = await resp.json()
                success = resp.status == 200 and "choices" in body
    except Exception as exc:
        # Non-fatal — just count as failure
        _ = exc

    elapsed_ms = (time.monotonic() - start) * 1000
    stats.record(success, elapsed_ms)

    progress_counter[0] += 1
    done = progress_counter[0]
    if done % 500 == 0 or done == total:
        pct = done / total * 100
        print(f"  [{done:>5}/{total}] {pct:5.1f}% complete  "
              f"| success={stats.success}  fail={stats.failure}  "
              f"| last_latency={elapsed_ms:.0f}ms")


# ─────────────────────────────────────────────────────────────────────────────
# Kafka topic message count check
# ─────────────────────────────────────────────────────────────────────────────

def check_kafka_topic(kafka_ui_url: str, topic: str, wait_seconds: int = 15) -> None:
    """Poll the Kafka UI REST API to report message count on the topic."""
    print(f"\nWaiting {wait_seconds}s for the Kafka connector to flush trace records…")
    time.sleep(wait_seconds)

    url = f"{kafka_ui_url}/api/clusters/local/topics/{topic}/messages?limit=1&seekType=BEGINNING"
    try:
        req = urllib.request.Request(url, headers={"Accept": "application/json"})
        with urllib.request.urlopen(req, timeout=10) as resp:
            data = json.loads(resp.read().decode())
            total_msgs = data.get("totalElapsedMs", "unknown")
            print(f"\n  Kafka topic '{topic}': API reachable.")
            print(f"  → Browse messages at: {kafka_ui_url}/ui/clusters/local/all-topics/{topic}/messages")
    except urllib.error.URLError:
        print(f"\n  Could not reach Kafka UI at {kafka_ui_url}.")
        print(f"  → Open {kafka_ui_url} in your browser and inspect topic '{topic}'.")

    # Also try the topics summary endpoint
    try:
        req2 = urllib.request.Request(
            f"{kafka_ui_url}/api/clusters/local/topics/{topic}",
            headers={"Accept": "application/json"},
        )
        with urllib.request.urlopen(req2, timeout=10) as resp2:
            topic_data = json.loads(resp2.read().decode())
            partitions = topic_data.get("partitions", [])
            total_messages = sum(
                p.get("offsetMax", 0) - p.get("offsetMin", 0) for p in partitions
            )
            print(f"  → Estimated messages in topic: {total_messages}")
    except Exception:
        pass


# ─────────────────────────────────────────────────────────────────────────────
# Main entry point
# ─────────────────────────────────────────────────────────────────────────────

async def run_load_test(
    total: int, concurrency: int, bifrost_url: str, kafka_ui_url: str, kafka_topic: str
):
    print("=" * 60)
    print("  Bifrost Load Test")
    print("=" * 60)
    print(f"  Target          : {bifrost_url}")
    print(f"  Total requests  : {total}")
    print(f"  Concurrency     : {concurrency}")
    print(f"  Kafka UI        : {kafka_ui_url}")
    print(f"  Kafka topic     : {kafka_topic}")
    print("=" * 60 + "\n")

    stats = Stats()
    semaphore = asyncio.Semaphore(concurrency)
    progress_counter = [0]

    connector = aiohttp.TCPConnector(limit=concurrency + 10)
    async with aiohttp.ClientSession(connector=connector) as session:
        start_time = time.monotonic()
        tasks = [
            send_request(session, bifrost_url, i, stats, semaphore, progress_counter, total)
            for i in range(total)
        ]
        await asyncio.gather(*tasks)
        elapsed = time.monotonic() - start_time

    stats.print_summary(elapsed)

    check_kafka_topic(kafka_ui_url, kafka_topic)

    print(f"\n  Kafka UI dashboard : {kafka_ui_url}")
    print(f"  Bifrost dashboard  : {bifrost_url}\n")

    return stats.failure == 0


def main():
    parser = argparse.ArgumentParser(description="Bifrost load test")
    parser.add_argument("--total", type=int, default=TOTAL_REQUESTS_DEFAULT)
    parser.add_argument("--concurrency", type=int, default=CONCURRENCY_DEFAULT)
    parser.add_argument("--bifrost-url", default=BIFROST_URL_DEFAULT)
    parser.add_argument("--kafka-ui-url", default=KAFKA_UI_URL_DEFAULT)
    parser.add_argument("--kafka-topic", default=TOPIC_NAME_DEFAULT)
    args = parser.parse_args()

    success = asyncio.run(
        run_load_test(
            args.total,
            args.concurrency,
            args.bifrost_url,
            args.kafka_ui_url,
            args.kafka_topic,
        )
    )
    sys.exit(0 if success else 1)


if __name__ == "__main__":
    main()
