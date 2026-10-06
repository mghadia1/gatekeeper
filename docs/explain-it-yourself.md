# Explain it yourself

Questions to answer out loud, without notes, before Gatekeeper goes near a
resume or an interview. There are no answers here: the explanation has to
be yours. Gatekeeper was built with Claude ahead of the Go ramp, so this
matters more than usual; and Go stays off the resume until the ramp's gate
(`projects/go-ramp/GATE.md`) passes.

## Run it first

1. `docker compose up --build --detach --wait`, then
   `./scripts/smoke.sh`. Pick three of its checks and say what each one
   proves about the gateway.
2. Stop `users-2` with `docker compose stop users-2` and send ten requests
   to `/users/1`. Why did none fail, and what would have happened without
   retries?

## Design choices (pick one to defend)

3. Why does the gateway use Go's `ServeMux` patterns instead of its own
   router? What happens when two configured routes conflict, and where does
   that become an error? (`internal/gateway/gateway.go`)
4. Why is the rate limit in Redis, and why is the token-bucket step a Lua
   script instead of a GET and a SET? What does using Redis's clock protect
   against? (`internal/ratelimit/redis.go`)
5. When is a failed request retried, and when not? Why is a `POST` with a
   body never retried, but a bodyless `POST` whose connection was refused
   is? (`internal/gateway/retry.go`)
6. What does the retry budget prevent? Explain "150 attempts instead of
   600".
7. The circuit breaker and instance ejection both react to failures. What
   different problem does each solve?
8. Why are metrics on port 9091 and not 8080, and why is the route label a
   pattern and never a path?

## Failure modes (pick one to tell as a story)

9. Why did a catch-all 404 handler make 405s disappear?
10. Why did "fail open" take 1.7 seconds per request while Redis was down,
    and why is `MaxRetries` set to `-1` rather than `0`?
11. Why were rate-limited requests counted as `unmatched`?

## Numbers you should be able to source

12. "One API key across two replicas got exactly 10 of 60 requests": where
    does that come from, and why is 10 the right number?
13. The rate-limit accuracy test let 549 requests through, and the runner
    expected 550. Which one was wrong, and why is 549 the right number?
14. Why does the load test report no figure for the gateway's added
    latency, and no p99? What in `bench/RESULTS.md` rules each one out,
    and what setup would let you measure them?
15. At 4,000/s the gateway with the Redis limiter had a lower median than
    calling the backend directly. What was tested, what did it rule out,
    and how would you answer "so why was it faster?" in an interview?
