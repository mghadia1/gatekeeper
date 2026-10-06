# Load test results

One run, 2026-10-06, at commit 049470c: `bench/results/20261006T210408Z/`.
MacBook Air M2, 8 GB; Docker Desktop VM with 8 CPUs and 3.8 GB; the load
generator runs in the same Compose network, on the same laptop, as the
gateway, Redis and the echo backend. Every configuration ran twice (forward,
then reverse order), 15 s measured after 3 s of warm-up. The host's swap
check passed before the start (0 MB in 10 s) and on every run (at most
17.6 MB swapped in, against a 50 MB limit). **This is a laptop, not a
server benchmark.**

## What it shows

**1. The shared rate limit is exact.** One API key sent 300 requests/s for
10 s, alternating between the two gateway replicas, against a limit of
50/s with a burst of 50. A token bucket shared through Redis should let
through the burst plus what refills between the first request and the last
(2,999 / 300 = 9.997 s): 50 + 499.8, so **549** whole requests. Both runs
let through **549**, refused 2,451, and had 0 errors. Two replicas did not
double the limit, and there was not one request to spare.

The runner printed "expected 550 (-0.18%)": its formula used the full
10 s instead of the time between the first and last request. That is fixed
in `run.py` for later runs; this run's files are left as written.

**2. 4,000 requests per second, without errors.** Through the gateway, and
through the gateway with the Redis limiter on, every rate up to 4,000/s was
delivered on schedule with all 195,000 measured requests per target
answered 200, 0 errors and 0 dropped. That is a floor, not a maximum:
higher rates were not tried, and on one laptop the load generator shares
the CPUs, so a maximum would measure the laptop as much as the gateway.

## What it does not show

**The gateway's added latency.** Medians through the gateway were
0.71-1.46 ms; directly to the backend, 0.74-1.19 ms. At 500/s the order is
the expected one (direct 1.01 ms mean, gateway 1.37, gateway with the
limiter 1.57), but the direct path alone moved from 0.82 to 1.19 ms
between its two passes, as much as the difference being measured. So the
gateway's cost is below what this setup can resolve, and no overhead figure
is claimed.

**Tail latency.** p99 swung from 3.74 ms to 117 ms between two passes of
the same configuration, and p99.9 reached 188 ms, in runs the swap check
accepted. Pauses of 100 ms or more on a busy laptop decide those numbers,
not the gateway, so no p99 is claimed.

## Unexplained

**At 4,000/s the gateway with the Redis limiter had a median of 0.37 ms in
both passes**, against 0.71-0.81 ms for the gateway without it and
0.74-0.90 ms direct. A request that does strictly more work (a Redis round
trip, then the same proxying) should not be faster. The configs differ only
in the `rate_limit` block, and both passes agree, so it is not a one-off.

One explanation was tested and rejected: that extra work keeps CPUs from
going idle, and waking an idle CPU in Docker's VM is slow.
`bench/idle_check.py` ran the direct path at 4,000/s with and without a
busy-loop container: the median got *slower* with it (0.69 to 0.90 ms), so
that does not explain a faster median. The p99 did drop sharply (6.6-9.0 ms
to 2.24 ms), which fits idle wake-ups causing slow outliers.
(`results/20261006T210408Z/idle-check.txt`.)

The cause is not known. A next step would be a CPU profile of the gateway
and the load generator under both configs, or the same run on a Linux
machine without Docker Desktop's VM.

## Full table

| Target | Rate (req/s) | p50 ms (pass 1, 2) | p99 ms (pass 1, 2) | Errors | Dropped |
|---|---|---|---|---|---|
| direct | 500 | 0.82, 1.19 | 2.78, 2.61 | 0 | 0 |
| direct | 2000 | 1.09, 1.00 | 9.49, 6.68 | 0 | 0 |
| direct | 4000 | 0.90, 0.74 | 10.69, 7.03 | 0 | 0 |
| gateway | 500 | 1.27, 1.46 | 3.74, 117.22 | 0 | 0 |
| gateway | 2000 | 1.03, 1.05 | 3.60, 7.22 | 0 | 0 |
| gateway | 4000 | 0.81, 0.71 | 3.51, 7.24 | 0 | 0 |
| gateway+limit | 500 | 1.60, 1.54 | 4.15, 20.95 | 0 | 0 |
| gateway+limit | 2000 | 1.01, 0.89 | 3.31, 3.21 | 0 | 0 |
| gateway+limit | 4000 | 0.37, 0.37 | 9.05, 2.29 | 0 | 0 |

Every request in every run got a 200. The p99 column is there for
completeness; see above for why it is not a result.
