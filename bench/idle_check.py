"""Does keeping one Docker VM CPU busy make the direct path faster?

    python3 bench/idle_check.py      # about 2 minutes

Tests one explanation for the 2026-10-06 run's oddity, gateway+limit
(0.37 ms p50 at 4000/s) beating the direct path (0.82 ms): extra work keeps
CPUs from idling, and waking an idle vCPU is slow. Direct at 4000/s,
alternating without/with a busy-loop container, two rounds.

Result (bench/RESULTS.md): the median got slower with the busy loop, so this
does not explain it; the p99 got much better.
"""

import argparse
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "bench"))
import run  # noqa: E402

args = argparse.Namespace(duration=15, warmup=3)
stack = run.Stack()
run.sh(*run.COMPOSE, "up", "--detach", "--wait", env=stack.env("bench/configs/plain.json"))
stack.config = "bench/configs/plain.json"

results = []
try:
    for round_ in (1, 2):
        for spinner in (False, True):
            if spinner:
                subprocess.run(["docker", "run", "--rm", "--detach", "--name", "gk-spin", "redis:8-alpine",
                                "sh", "-c", "while :; do :; done"], check=True, capture_output=True)
            try:
                print(f"round {round_}, busy loop {'on' if spinner else 'off'}:", flush=True)
                res = run.measure(stack, "direct", 4000, args, round_)
                results.append((round_, spinner, res["latency_ms"]["p50"], res["latency_ms"]["p99"], res["valid"]))
            finally:
                if spinner:
                    subprocess.run(["docker", "rm", "--force", "gk-spin"], capture_output=True)
finally:
    run.sh(*run.COMPOSE, "down", "--volumes", "--timeout", "5")

print("\nround  busy-loop  p50 ms  p99 ms  valid")
for r in results:
    print(f"{r[0]:5d}  {'on ' if r[1] else 'off'}        {r[2]:6.2f}  {r[3]:6.2f}  {r[4]}")
