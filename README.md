# Gatekeeper

An HTTP API gateway, written in Go: one entry point in front of several
backend services that routes requests, spreads load across healthy
instances, rate-limits clients, survives backend failures, and shows what it
is doing through metrics.

> **Status:** Milestones 0-3 of 7. It routes requests from a config file,
> spreads them across a backend's healthy instances, and rate-limits each
> client with a limit shared by every gateway replica. Circuit breakers,
> retries, metrics and the load test are planned, not built.

## Status

| # | Milestone | Status |
|---|-----------|--------|
| 0 | Skeleton: Go module, CI, `/healthz`, graceful shutdown, distroless image, Compose | Built; waiting on CI |
| 1 | Routing: config file, method + path-pattern matching, reverse proxy, request IDs, timeouts | Built; waiting on CI |
| 2 | Load balancing, active health checks, passive failure detection | Built; waiting on CI |
| 3 | Rate limiting: token bucket per client, shared across replicas through Redis | Built; waiting on CI |
| 4 | Resilience: circuit breakers, retries for idempotent requests, body limits | Not started |
| 5 | Observability: Prometheus metrics, JSON logs | Not started |
| 6 | Load test: gateway overhead, throughput, rate-limit accuracy under load | Not started |
| 7 | Architecture diagram, design decisions, benchmarks | Not started |

## Quickstart

Needs Docker.

```bash
docker compose up --build --detach --wait
curl http://localhost:8080/users/42          # routed to the "users" backend
curl -X DELETE http://localhost:8080/api/orders/7   # to "orders", as /orders/7
docker compose down
```

Compose runs two gateway replicas (8080 and 8081) sharing a rate limit
through Redis, in front of test backends (all `cmd/echo`, which answers with
a JSON description of what reached it): `users` with two instances, and
`orders`, using `config/gatekeeper.json`.

## Configuration

```json
{
  "default_timeout": "5s",
  "backends": {
    "users": {
      "urls": ["http://users-1:9000", "http://users-2:9000"],
      "health_check": {"path": "/healthz", "interval": "1s", "unhealthy_after": 2, "healthy_after": 2},
      "eject_after": 3,
      "eject_for": "10s"
    },
    "users-strict": {"url": "http://users-1:9000", "timeout": "500ms"},
    "orders": {"url": "http://orders:9000"}
  },
  "routes": [
    {"match": "GET /users/{id}", "backend": "users"},
    {"match": "POST /users", "backend": "users"},
    {"match": "/api/orders/", "backend": "orders", "strip_prefix": "/api"},
    {"match": "GET /slow", "backend": "users-strict"}
  ],
  "rate_limit": {
    "requests_per_second": 5, "burst": 10,
    "key_header": "X-API-Key",
    "store": "redis", "redis_url": "redis://redis:6379/0",
    "on_store_error": "allow"
  }
}
```

- `match` is a Go `net/http` routing pattern: an optional method, a path
  with `{name}` wildcards, a trailing `{name...}`, or a trailing `/` for
  everything below. When two routes match, the more specific one wins
  (`GET /users/me` beats `GET /users/{id}`).
- `strip_prefix` is removed from the path before forwarding.
- `timeout` bounds the whole exchange with that backend.
- `url` for one instance or `urls` for several; `health_check` and
  `eject_after` / `eject_for` are explained under [Load balancing](#load-balancing).
- Unknown fields, unknown backends, non-HTTP URLs, non-positive timeouts and
  conflicting routes all stop the gateway at startup, with the file and the
  problem named.

## How a request is handled

1. It gets a request id: the caller's `X-Request-ID` if it is short and
   plain, otherwise a new random one.
2. The most specific matching route picks the backend. No match: JSON `404`.
   The path matches but the method does not: `405` with an `Allow` header.
3. The request is forwarded with `X-Forwarded-For`, `-Host`, `-Proto` and the
   request id, within the backend's timeout.
4. The answer goes back with `X-Request-ID` (exactly once, even if the
   backend echoes it). An unreachable backend gives `502`; one that does not
   answer in time gives `504` at the timeout.
5. One JSON access-log line: request id, method, path (without the query
   string, which may hold secrets), status, duration, bytes, route, backend,
   and the error if there was one.

The connection pool keeps up to 256 idle connections per backend. Go's
default is 2, which would make a busy proxy open and close a connection for
most requests.

**A bug found while building it.** A catch-all `/` route, added to answer
unmatched paths with a JSON 404, also caught requests whose path matched a
route but whose method did not, so every `405` became a `404`. `ServeMux`
only answers 405 when no pattern matches the path under any method. The
fallback now asks the router which methods would match, and answers 405
with `Allow` when some would.

## Load balancing

A backend's requests go round-robin to its healthy instances. Two signals
decide what "healthy" means:

- **Active health checks** (if `health_check` is set): every `interval`, the
  gateway requests `path` on each instance. An instance is marked down after
  `unhealthy_after` failed checks in a row and up again after `healthy_after`
  passes, so one slow check does not flip it.
- **Passive detection** (always): a request that fails to connect, times
  out, or gets a `503` counts against its instance; after `eject_after` in a
  row, the instance sits out for `eject_for`. Other 5xx answers do not count:
  a `500` is about that one request, not the instance.

If every instance looks unhealthy, the gateway sends traffic to all of them
anyway ("fail open", like Envoy's panic mode). A health check that is wrong,
say a broken `/healthz`, should not turn into a total outage; if the
instances really are down, requests fail with `502` either way.

Measured in the tests: with one of two instances dead and no health checks,
exactly `eject_after` requests fail (2 of 40) before passive detection takes
it out; the rest succeed. With health checks, the smoke test stops `users-2`,
and after 3 s all requests go to `users-1` with none failing; restarted, it
is back in rotation. There are no retries yet, so a request already sent to
a dying instance fails (retries come in Milestone 4).

**A bug caught before it shipped.** The health checks run beside the server
and must stop with it. The first version waited for them in a `defer`
registered after the signal handler's `stop()`; deferred calls run in
reverse order, so on a startup failure it would have waited for checks that
only stop when `stop()` runs, and hung. It now cancels the checks itself
before waiting.

## Rate limiting

Each client gets a token bucket: up to `burst` requests at once, refilled at
`requests_per_second`. A refused request gets `429` with `Retry-After` (whole
seconds, rounded up); every limited response carries `X-RateLimit-Limit` and
`X-RateLimit-Remaining`. Only proxied routes are limited; `/healthz` and
404s are not.

- **Who is a client.** The API key in `key_header` if the request has one,
  hashed (keys never reach Redis or the logs); otherwise the connection's IP.
  `X-Forwarded-For` is not trusted, since any client can set it.
- **Fractional tokens.** At 10 per second, half a token is back 50 ms after
  the bucket empties. A version that counted whole tokens and reset its
  clock on every request would starve a client that asks at exactly the
  refill rate; a test checks such a client is never refused.
- **`store: "memory"`** keeps buckets in the process, sweeping ones idle long
  enough to be full again, so memory does not grow with every client ever
  seen. Each replica has its own, so N replicas allow N times the limit.
- **`store: "redis"`** shares the buckets. One Lua script does the whole
  refill-and-take inside Redis, so two replicas cannot both spend the last
  token, and it uses Redis's clock, not the replicas'. Keys expire once a
  bucket would be full again. The token count is stored as a string: Lua
  numbers returned to Redis are truncated to integers.
- **If Redis fails**, `on_store_error` decides: `allow` (the default: the
  limiter is protection, and a Redis outage should not take the API down)
  or `deny` (503). After a failure the limiter skips Redis for 1 s, and logs
  the outage at most every 5 s.

Measured: in the smoke test, 60 requests with one key, alternating between
the two replicas as fast as they would go (0.1 s), got **10 through and 50
refused**: exactly the burst, once, not once per replica. With Redis
stopped, requests went through. The same Lua script runs against a real
Redis in CI, with two clients as two replicas taking from one bucket at
once.

**A bug found while building it.** The first fail-open path took about
**1.7 s per request** while Redis was down: go-redis retries a failed
command 3 times and a failed dial 5 times, with backoff, by default. Failing
open but slowly is close to failing. The limiter's client now has retries
off (`MaxRetries` is `-1`; `0` means "use the default"), plus the 1 s
skip, and a test requires each request during an outage to finish in under
300 ms. go-redis also printed its own plain-text log lines; they now go
through the gateway's JSON logger.

## The server itself

- `GET /healthz` is liveness only: the process is up and serving. It checks
  nothing else, so a backend outage never makes Docker restart the gateway.
- **Graceful shutdown.** On SIGTERM (what `docker stop` sends) the gateway
  stops accepting connections and lets requests in flight finish, up to
  `GATEKEEPER_SHUTDOWN_TIMEOUT` (default 10 s), then exits 0. Tested both
  ways: a slow request finishes; a stuck one is cut off at the timeout.
- **About 16 MB per image** (gateway and echo): a static binary in a
  distroless base, no shell or package manager, running as a non-root user. With no curl inside, the
  container health check runs the binary itself (`gatekeeper healthcheck`).
- JSON logs on stdout (`log/slog`).
- `ReadHeaderTimeout` is set, so a client that trickles headers cannot hold a
  connection open indefinitely.

## Development

Needs Go 1.27.

```bash
make test        # go test -race ./...
make test-redis  # the rate limiter's Lua script against Compose's Redis
make lint    # gofmt and go vet
make smoke   # start the stack; check routing, ids, 404/405/502/504 and a clean SIGTERM stop
```

CI runs gofmt, vet, `go test -race` (with a Redis service, so the rate
limiter's integration test runs; without it the test skips locally and fails
in CI) and a build, then a Compose job that starts the stack and runs the
smoke test over real HTTP.

## License

MIT.
