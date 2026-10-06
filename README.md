# Gatekeeper

An HTTP API gateway, written in Go: one entry point in front of several
backend services that routes requests, spreads load across healthy
instances, rate-limits clients, survives backend failures, and shows what it
is doing through metrics.

> **Status:** Milestones 0-1 of 7. It routes requests to backends from a
> config file. Load balancing, rate limiting, circuit breakers, metrics and
> the load test are planned, not built.

## Status

| # | Milestone | Status |
|---|-----------|--------|
| 0 | Skeleton: Go module, CI, `/healthz`, graceful shutdown, distroless image, Compose | Built; waiting on CI |
| 1 | Routing: config file, method + path-pattern matching, reverse proxy, request IDs, timeouts | Built; waiting on CI |
| 2 | Load balancing, active health checks, passive failure detection | Not started |
| 3 | Rate limiting: token bucket per client, shared across replicas through Redis | Not started |
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

Compose runs the gateway in front of two test backends, `users` and
`orders` (both `cmd/echo`, which answers with a JSON description of what
reached it), using `config/gatekeeper.json`.

## Configuration

```json
{
  "default_timeout": "5s",
  "backends": {
    "users": {"url": "http://users:9000"},
    "users-strict": {"url": "http://users:9000", "timeout": "500ms"},
    "orders": {"url": "http://orders:9000"}
  },
  "routes": [
    {"match": "GET /users/{id}", "backend": "users"},
    {"match": "POST /users", "backend": "users"},
    {"match": "/api/orders/", "backend": "orders", "strip_prefix": "/api"},
    {"match": "GET /slow", "backend": "users-strict"}
  ]
}
```

- `match` is a Go `net/http` routing pattern: an optional method, a path
  with `{name}` wildcards, a trailing `{name...}`, or a trailing `/` for
  everything below. When two routes match, the more specific one wins
  (`GET /users/me` beats `GET /users/{id}`).
- `strip_prefix` is removed from the path before forwarding.
- `timeout` bounds the whole exchange with that backend.
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
make test    # go test -race ./...
make lint    # gofmt and go vet
make smoke   # start the stack; check routing, ids, 404/405/502/504 and a clean SIGTERM stop
```

CI runs gofmt, vet, `go test -race` and a build, then a Compose job that
starts the stack and runs the smoke test over real HTTP.

## License

MIT.
