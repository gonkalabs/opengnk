#!/usr/bin/env python3
"""
Zero-dependency stress tester for an OpenAI-compatible chat completions endpoint.

Designed for the opengnk proxy running on http://localhost:8080.

Examples:
    # 50 requests, 10 in flight, non-streaming
    python3 stress.py -c 10 -n 50

    # 30 second soak, 8 in flight, streaming, capture TTFT
    python3 stress.py -c 8 -d 30 --stream

    # Hit a different host / model
    python3 stress.py --url http://localhost:8080/v1/chat/completions \
                      --model Qwen/Qwen3-235B-A22B-Instruct-2507-FP8 \
                      -c 4 -n 20
"""

import argparse
import json
import random
import statistics
import sys
import threading
import time
import urllib.request
import urllib.error
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from typing import Optional

PROMPTS = [
    "Explain quantum entanglement in two sentences.",
    "Write a haiku about distributed systems.",
    "What is the capital of Mongolia and one fun fact about it?",
    "Give me three creative startup ideas for 2026.",
    "Summarise the plot of Moby Dick in 50 words.",
    "Translate 'good morning' into five different languages.",
    "List five tips for writing efficient Go code.",
    "What's the difference between TCP and UDP?",
    "Compose a short limerick about a tired programmer.",
    "Name three lesser-known programming languages and what they're good for.",
    "Describe the colour blue to someone who has never seen.",
    "Give me a one-paragraph history of the Roman Empire.",
]


@dataclass
class Result:
    ok: bool
    status: int
    latency_s: float
    ttft_s: Optional[float] = None
    prompt_tokens: int = 0
    completion_tokens: int = 0
    error: str = ""


@dataclass
class Counters:
    started: int = 0
    done: int = 0
    ok: int = 0
    fail: int = 0
    lock: threading.Lock = field(default_factory=threading.Lock)


def make_payload(model: str, max_tokens: int, stream: bool) -> dict:
    return {
        "model": model,
        "messages": [{"role": "user", "content": random.choice(PROMPTS)}],
        "max_tokens": max_tokens,
        "stream": stream,
    }


def do_request(url: str, model: str, max_tokens: int, stream: bool, timeout: float) -> Result:
    body = json.dumps(make_payload(model, max_tokens, stream)).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=body,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "Accept": "text/event-stream" if stream else "application/json",
            "Authorization": "Bearer not-needed",
        },
    )

    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            status = resp.status
            if not stream:
                raw = resp.read()
                latency = time.perf_counter() - t0
                if status >= 400:
                    return Result(False, status, latency, error=raw[:200].decode("utf-8", "replace"))
                try:
                    data = json.loads(raw)
                except Exception as e:
                    return Result(False, status, latency, error=f"json: {e}")
                usage = data.get("usage") or {}
                return Result(
                    ok=True,
                    status=status,
                    latency_s=latency,
                    prompt_tokens=int(usage.get("prompt_tokens", 0) or 0),
                    completion_tokens=int(usage.get("completion_tokens", 0) or 0),
                )

            ttft = None
            prompt_tokens = 0
            completion_tokens = 0
            for raw_line in resp:
                line = raw_line.decode("utf-8", "replace").strip()
                if not line.startswith("data:"):
                    continue
                payload = line[5:].strip()
                if payload == "[DONE]":
                    break
                if ttft is None:
                    ttft = time.perf_counter() - t0
                try:
                    chunk = json.loads(payload)
                except Exception:
                    continue
                usage = chunk.get("usage")
                if usage:
                    prompt_tokens = int(usage.get("prompt_tokens", 0) or 0)
                    completion_tokens = int(usage.get("completion_tokens", 0) or 0)
                else:
                    for ch in chunk.get("choices") or []:
                        delta = ch.get("delta") or {}
                        if delta.get("content"):
                            completion_tokens += 1  # rough chunk count if usage missing
            latency = time.perf_counter() - t0
            return Result(
                ok=True,
                status=status,
                latency_s=latency,
                ttft_s=ttft,
                prompt_tokens=prompt_tokens,
                completion_tokens=completion_tokens,
            )
    except urllib.error.HTTPError as e:
        latency = time.perf_counter() - t0
        body = ""
        try:
            body = e.read()[:200].decode("utf-8", "replace")
        except Exception:
            pass
        return Result(False, e.code, latency, error=f"HTTP {e.code}: {body}")
    except Exception as e:
        latency = time.perf_counter() - t0
        return Result(False, 0, latency, error=f"{type(e).__name__}: {e}")


def percentile(values, p):
    if not values:
        return 0.0
    s = sorted(values)
    k = (len(s) - 1) * (p / 100.0)
    f = int(k)
    c = min(f + 1, len(s) - 1)
    if f == c:
        return s[f]
    return s[f] + (s[c] - s[f]) * (k - f)


def progress_loop(counters: Counters, total: Optional[int], duration: Optional[float], started_at: float, stop_evt: threading.Event):
    last_done = 0
    last_t = started_at
    while not stop_evt.is_set():
        time.sleep(1.0)
        with counters.lock:
            done = counters.done
            ok = counters.ok
            fail = counters.fail
            started = counters.started
        now = time.perf_counter()
        rps = (done - last_done) / max(now - last_t, 1e-9)
        elapsed = now - started_at
        msg = f"  [t={elapsed:6.1f}s] in-flight={started - done:3d} done={done} ok={ok} fail={fail} ({rps:5.2f} req/s)"
        if total:
            msg += f"  {done}/{total}"
        elif duration:
            msg += f"  remaining={max(0.0, duration - elapsed):.1f}s"
        print(msg, flush=True)
        last_done = done
        last_t = now


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--url", default="http://localhost:8080/v1/chat/completions")
    ap.add_argument("--model", default="Qwen/Qwen3-235B-A22B-Instruct-2507-FP8")
    ap.add_argument("-c", "--concurrency", type=int, default=4, help="number of in-flight requests")
    ap.add_argument("-n", "--requests", type=int, default=0, help="total requests to send (0 = use --duration)")
    ap.add_argument("-d", "--duration", type=float, default=0.0, help="seconds to keep generating load (used if -n is 0)")
    ap.add_argument("--max-tokens", type=int, default=128)
    ap.add_argument("--stream", action="store_true", help="use SSE streaming and capture TTFT")
    ap.add_argument("--timeout", type=float, default=120.0)
    ap.add_argument("--seed", type=int, default=None)
    args = ap.parse_args()

    if args.requests <= 0 and args.duration <= 0:
        args.requests = 20

    if args.seed is not None:
        random.seed(args.seed)

    print("=" * 72)
    print("opengnk stress test")
    print("=" * 72)
    print(f"  url           : {args.url}")
    print(f"  model         : {args.model}")
    print(f"  concurrency   : {args.concurrency}")
    if args.requests:
        print(f"  total requests: {args.requests}")
    else:
        print(f"  duration      : {args.duration}s")
    print(f"  max_tokens    : {args.max_tokens}")
    print(f"  stream        : {args.stream}")
    print(f"  timeout       : {args.timeout}s")
    print("=" * 72, flush=True)

    counters = Counters()
    results: list[Result] = []
    results_lock = threading.Lock()
    started_at = time.perf_counter()
    stop_evt = threading.Event()

    progress_thread = threading.Thread(
        target=progress_loop,
        args=(counters, args.requests or None, args.duration or None, started_at, stop_evt),
        daemon=True,
    )
    progress_thread.start()

    def task(_idx: int) -> Result:
        with counters.lock:
            counters.started += 1
        r = do_request(args.url, args.model, args.max_tokens, args.stream, args.timeout)
        with counters.lock:
            counters.done += 1
            if r.ok:
                counters.ok += 1
            else:
                counters.fail += 1
        with results_lock:
            results.append(r)
        return r

    with ThreadPoolExecutor(max_workers=args.concurrency) as pool:
        if args.requests > 0:
            futures = [pool.submit(task, i) for i in range(args.requests)]
            for _ in as_completed(futures):
                pass
        else:
            deadline = started_at + args.duration
            inflight = set()
            next_idx = 0
            while True:
                while len(inflight) < args.concurrency and time.perf_counter() < deadline:
                    inflight.add(pool.submit(task, next_idx))
                    next_idx += 1
                if not inflight:
                    break
                done, inflight = next_wait(inflight)
                if time.perf_counter() >= deadline and not inflight:
                    break

    stop_evt.set()
    progress_thread.join(timeout=2.0)
    elapsed = time.perf_counter() - started_at

    print()
    print("=" * 72)
    print("results")
    print("=" * 72)

    n = len(results)
    ok_results = [r for r in results if r.ok]
    fail_results = [r for r in results if not r.ok]
    latencies = [r.latency_s for r in ok_results]
    ttfts = [r.ttft_s for r in ok_results if r.ttft_s is not None]
    prompt_tokens = sum(r.prompt_tokens for r in ok_results)
    completion_tokens = sum(r.completion_tokens for r in ok_results)

    print(f"  total         : {n}")
    print(f"  ok            : {len(ok_results)}")
    print(f"  failed        : {len(fail_results)}")
    print(f"  wall time     : {elapsed:.2f}s")
    print(f"  throughput    : {n / max(elapsed, 1e-9):.2f} req/s")
    if ok_results:
        print(f"  ok throughput : {len(ok_results) / max(elapsed, 1e-9):.2f} req/s")
    if completion_tokens:
        print(f"  prompt toks   : {prompt_tokens}")
        print(f"  output toks   : {completion_tokens}  ({completion_tokens / max(elapsed, 1e-9):.1f} tok/s aggregate)")

    if latencies:
        print()
        print("  latency (s)   : "
              f"min={min(latencies):.2f}  "
              f"avg={statistics.fmean(latencies):.2f}  "
              f"p50={percentile(latencies, 50):.2f}  "
              f"p95={percentile(latencies, 95):.2f}  "
              f"p99={percentile(latencies, 99):.2f}  "
              f"max={max(latencies):.2f}")
    if ttfts:
        print(f"  TTFT (s)      : "
              f"min={min(ttfts):.2f}  "
              f"avg={statistics.fmean(ttfts):.2f}  "
              f"p50={percentile(ttfts, 50):.2f}  "
              f"p95={percentile(ttfts, 95):.2f}  "
              f"p99={percentile(ttfts, 99):.2f}  "
              f"max={max(ttfts):.2f}")

    if fail_results:
        print()
        print(f"  failures (showing up to 5):")
        for r in fail_results[:5]:
            print(f"    status={r.status} latency={r.latency_s:.2f}s err={r.error[:160]}")
        # Failure breakdown
        bucket: dict[str, int] = {}
        for r in fail_results:
            key = f"status={r.status}" if r.status else (r.error.split(":", 1)[0] or "unknown")
            bucket[key] = bucket.get(key, 0) + 1
        print(f"  failure types : {bucket}")

    print("=" * 72)
    sys.exit(0 if not fail_results else 1)


def next_wait(inflight):
    """Block until at least one future completes; return (done, still_running)."""
    while True:
        done = {f for f in inflight if f.done()}
        if done:
            return done, inflight - done
        time.sleep(0.05)


if __name__ == "__main__":
    main()
