# Gatekeeper

An HTTP API gateway, written in Go: one entry point in front of several
backend services that routes requests, spreads load across healthy
instances, rate-limits clients, survives backend failures, and shows what it
is doing through metrics.

> **Status:** Milestone 0 of 7. Today it serves a liveness check and shuts
> down gracefully; everything below the first row of the table is planned,
> not built.

## Status

| # | Milestone | Status |
|---|-----------|--------|
| 0 | Skeleton: Go module, CI, `/healthz`, graceful shutdown, distroless image, Compose | Built; waiting on CI |
| 1 | Routing: config file, method + path-pattern matching, reverse proxy, request IDs, timeouts | Not started |
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
curl http://localhost:8080/healthz     # {"status":"ok","version":"dev"}
docker compose down
```

## What is there now

- `GET /healthz` is liveness only: the process is up and serving. It checks
  nothing else, so a backend outage never makes Docker restart the gateway.
- **Graceful shutdown.** On SIGTERM (what `docker stop` sends) the gateway
  stops accepting connections and lets requests in flight finish, up to
  `GATEKEEPER_SHUTDOWN_TIMEOUT` (default 10 s), then exits 0. Tested both
  ways: a slow request finishes; a stuck one is cut off at the timeout.
- **A 15.7 MB image**: a static binary in a distroless base, no shell or
  package manager, running as a non-root user. With no curl inside, the
  container health check runs the binary itself (`gatekeeper healthcheck`).
- JSON logs on stdout (`log/slog`).
- `ReadHeaderTimeout` is set, so a client that trickles headers cannot hold a
  connection open indefinitely.

## Development

Needs Go 1.27.

```bash
make test    # go test -race ./...
make lint    # gofmt and go vet
make smoke   # build the image, start it, check /healthz and a clean SIGTERM stop
```

CI runs gofmt, vet, `go test -race` and a build, then a Compose job that
starts the image and runs the smoke test.

## License

MIT.
