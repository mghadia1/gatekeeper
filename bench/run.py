"""Gatekeeper's load test.

    python3 bench/run.py [--duration 15] [--warmup 3] [--rates 500,2000,4000]

Runs cmd/loadgen inside the Compose network (so Docker Desktop's port
forwarding is not measured) against three targets at each rate:

- direct:          the echo backend itself, the baseline
- gateway:         through the gateway (bench/configs/plain.json)
- gateway+limit:   through the gateway with the Redis rate limiter on, at a
                   limit far above the load (bench/configs/limited.json), so
                   nothing is refused and only the limiter's cost is added

then a rate-limit accuracy test: one API key at 300 requests/s for 10 s,
alternating between both replicas, against a limit of 50/s with a burst of
50 (bench/configs/accuracy.json). The limit should let exactly
burst + rate x seconds through, wherever the requests land.

Method, from the SurgiServe benchmark: every (target, rate) runs twice, in
a forward and then a reverse pass, so a laptop that slows as it warms up
affects each configuration about equally; the runner refuses to start while
the host is swapping, and records the host's swap-ins during every run (a
run with swap-ins is marked invalid). Latency is measured open-loop, from
when each request was due (see cmd/loadgen).

Writes bench/results/<timestamp>/results.json and results.md.
"""

import argparse
import json
import os
import platform
import statistics
import subprocess
import sys
import time
from datetime import UTC, datetime
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
COMPOSE = ["docker", "compose", "-f", "compose.yaml", "-f", "bench/compose.bench.yaml"]
SWAP_TOLERANCE_MB = 50.0

TARGETS = {
    "direct": ("bench/configs/plain.json", "http://users-1:9000/users/1", ""),
    "gateway": ("bench/configs/plain.json", "http://gatekeeper:8080/users/1", ""),
    "gateway+limit": ("bench/configs/limited.json", "http://gatekeeper:8080/users/1", "X-API-Key: bench"),
}


def sh(*command: str, env: dict | None = None) -> str:
    return subprocess.run(command, cwd=ROOT, env=env, check=True, capture_output=True, text=True).stdout


def host_swap_mb() -> tuple[float, float]:
    out = sh("vm_stat")
    page = int(out.split("page size of ")[1].split(" bytes")[0])
    counts = {}
    for line in out.splitlines():
        name, _, value = line.partition(":")
        if name in ("Swapouts", "Swapins"):
            counts[name] = int(value.strip().rstrip(".")) * page / 2**20
    return counts["Swapouts"], counts["Swapins"]


class Stack:
    """The Compose stack, with the gateway config currently mounted."""

    def __init__(self) -> None:
        self.config: str | None = None

    def env(self, config: str) -> dict:
        return {**os.environ, "GATEKEEPER_BENCH_CONFIG": "./" + config}

    def use(self, config: str) -> None:
        if config == self.config:
            return
        sh(*COMPOSE, "up", "--detach", "--wait", "--force-recreate", "gatekeeper", "gatekeeper-2",
           env=self.env(config))
        self.config = config
        time.sleep(2)  # let the fresh gateways settle before measuring

    def loadgen(self, *args: str) -> dict:
        out = sh(*COMPOSE, "--profile", "bench", "run", "--rm", "--no-deps", "loadgen", *args,
                 env=self.env(self.config or "config/gatekeeper.json"))
        return json.loads(out.strip().splitlines()[-1])


def measure(stack: Stack, target: str, rate: int, args: argparse.Namespace, run_pass: int) -> dict:
    config, url, header = TARGETS[target]
    stack.use(config)
    out0, in0 = host_swap_mb()
    flags = ["-url", url, "-rate", str(rate), "-duration", f"{args.duration}s", "-warmup", f"{args.warmup}s"]
    if header:
        flags += ["-header", header]
    res = stack.loadgen(*flags)
    out1, in1 = host_swap_mb()
    res.update({
        "target": target, "rate": rate, "pass": run_pass,
        "host_swapped_out_mb": round(out1 - out0, 1), "host_swapped_in_mb": round(in1 - in0, 1),
        "valid": in1 - in0 <= SWAP_TOLERANCE_MB,
    })
    ok = res["statuses"].get("200", 0)
    print(f"  {target:14s} {rate:5d}/s pass {run_pass}: p50 {res['latency_ms']['p50']:7.2f} ms  "
          f"p99 {res['latency_ms']['p99']:8.2f} ms  ok {ok}/{res['requests']}  errors {res['errors']}  "
          f"dropped {res['dropped']}  swap-in {res['host_swapped_in_mb']} MB", flush=True)
    return res


def accuracy(stack: Stack, run_pass: int) -> dict:
    stack.use("bench/configs/accuracy.json")
    sh("docker", "compose", "exec", "-T", "redis", "redis-cli", "FLUSHALL")  # a full bucket to start
    out0, in0 = host_swap_mb()
    started = time.monotonic()
    res = stack.loadgen("-url", "http://gatekeeper:8080/users/1,http://gatekeeper-2:8080/users/1",
                        "-rate", "300", "-duration", "10s", "-warmup", "0s", "-header", "X-API-Key: accuracy")
    elapsed = time.monotonic() - started
    out1, in1 = host_swap_mb()
    allowed = res["statuses"].get("200", 0)
    refused = res["statuses"].get("429", 0)
    expected = 50 + 50 * 10  # burst + rate x the 10 s the load was sent for
    out = {
        "pass": run_pass, "allowed": allowed, "refused": refused, "errors": res["errors"],
        "requests": res["requests"], "expected_allowed": expected,
        "error_percent": round((allowed - expected) / expected * 100, 2),
        "wall_seconds": round(elapsed, 2),
        "host_swapped_in_mb": round(in1 - in0, 1), "valid": in1 - in0 <= SWAP_TOLERANCE_MB,
    }
    print(f"  accuracy pass {run_pass}: {allowed} allowed / {refused} refused of {res['requests']}; "
          f"expected {expected} ({out['error_percent']:+.2f}%)", flush=True)
    return out


def summary(runs: list[dict]) -> list[dict]:
    rows = []
    for target in TARGETS:
        for rate in sorted({r["rate"] for r in runs}):
            these = [r for r in runs if r["target"] == target and r["rate"] == rate]
            if not these:
                continue
            p50 = [r["latency_ms"]["p50"] for r in these]
            p99 = [r["latency_ms"]["p99"] for r in these]
            rows.append({
                "target": target, "rate": rate,
                "p50_ms_by_pass": p50, "p99_ms_by_pass": p99,
                "p50_ms": round(statistics.fmean(p50), 3), "p99_ms": round(statistics.fmean(p99), 3),
                "achieved_rate": round(statistics.fmean(r["achieved_rate"] for r in these), 1),
                "non_200": sum(r["requests"] - r["statuses"].get("200", 0) for r in these),
                "dropped": sum(r["dropped"] for r in these),
                "all_valid": all(r["valid"] for r in these),
            })
    return rows


def table(rows: list[dict]) -> str:
    lines = ["| Target | Rate (req/s) | p50 ms (pass 1, 2) | p99 ms (pass 1, 2) | Mean p50 | Mean p99 "
             "| Achieved req/s | Non-200 | Dropped | Valid |",
             "|---|---|---|---|---|---|---|---|---|---|"]
    for r in rows:
        lines.append(
            f"| {r['target']} | {r['rate']} | {', '.join(f'{v:.2f}' for v in r['p50_ms_by_pass'])} "
            f"| {', '.join(f'{v:.2f}' for v in r['p99_ms_by_pass'])} | {r['p50_ms']:.2f} | {r['p99_ms']:.2f} "
            f"| {r['achieved_rate']} | {r['non_200']} | {r['dropped']} | {'yes' if r['all_valid'] else 'NO'} |")
    return "\n".join(lines) + "\n"


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--duration", type=int, default=15)
    parser.add_argument("--warmup", type=int, default=3)
    parser.add_argument("--rates", default="500,2000,4000")
    parser.add_argument("--allow-swapping", action="store_true",
                        help="run even if the host is swapping (for checking the runner; runs are still flagged)")
    args = parser.parse_args()
    rates = [int(r) for r in args.rates.split(",")]

    before, _ = host_swap_mb()
    time.sleep(10)
    swapping = host_swap_mb()[0] - before
    print(f"host swapped out {swapping:.0f} MB in the last 10 s", flush=True)
    if swapping > SWAP_TOLERANCE_MB and not args.allow_swapping:
        sys.exit("the host is swapping; free memory and try again (or --allow-swapping to check the runner)")

    stamp = datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    out_dir = ROOT / "bench" / "results" / stamp
    out_dir.mkdir(parents=True)

    print("building and starting the stack", flush=True)
    stack = Stack()
    sh(*COMPOSE, "down", "--volumes", "--timeout", "5")
    sh(*COMPOSE, "--profile", "bench", "build", env=stack.env("bench/configs/plain.json"))
    sh(*COMPOSE, "up", "--detach", "--wait", env=stack.env("bench/configs/plain.json"))
    stack.config = "bench/configs/plain.json"

    plan = [(t, r) for t in TARGETS for r in rates]
    runs = []
    for run_pass, order in ((1, plan), (2, list(reversed(plan)))):
        print(f"pass {run_pass}", flush=True)
        for target, rate in order:
            runs.append(measure(stack, target, rate, args, run_pass))
    print("rate limit accuracy", flush=True)
    accuracy_runs = [accuracy(stack, 1), accuracy(stack, 2)]
    sh(*COMPOSE, "down", "--volumes", "--timeout", "5")

    info = json.loads(sh("docker", "info", "--format", "{{json .}}"))
    rows = summary(runs)
    record = {
        "environment": {
            "date": datetime.now(UTC).isoformat(timespec="seconds"),
            "git_commit": sh("git", "rev-parse", "--short", "HEAD").strip(),
            "machine": sh("sysctl", "-n", "hw.model").strip(),
            "chip": sh("sysctl", "-n", "machdep.cpu.brand_string").strip(),
            "memory_gb": int(sh("sysctl", "-n", "hw.memsize")) / 2**30,
            "macos": platform.mac_ver()[0],
            "docker_vm": {"cpus": info["NCPU"], "memory_gb": round(info["MemTotal"] / 2**30, 1)},
        },
        "settings": {"duration_s": args.duration, "warmup_s": args.warmup, "rates": rates,
                     "order": "forward pass, then reverse pass"},
        "summary": rows, "runs": runs, "accuracy": accuracy_runs,
    }
    (out_dir / "results.json").write_text(json.dumps(record, indent=2) + "\n")
    acc = "\n".join(
        f"- pass {a['pass']}: {a['allowed']} allowed, {a['refused']} refused, {a['errors']} errors; "
        f"expected {a['expected_allowed']} ({a['error_percent']:+.2f}%); valid: {'yes' if a['valid'] else 'NO'}"
        for a in accuracy_runs)
    (out_dir / "results.md").write_text(
        "## Latency\n\n" + table(rows) + "\n## Rate limit accuracy (50/s, burst 50, 300/s offered for 10 s, two replicas)\n\n"
        + acc + "\n")
    print(table(rows))
    print(acc)
    print(f"written to {out_dir}")


if __name__ == "__main__":
    main()
