.PHONY: build test test-redis lint up down smoke clean

build:
	go build -o bin/gatekeeper ./cmd/gatekeeper

# -race matters: the gateway handles every request on its own goroutine.
test:
	go test -race -count=1 ./...

# The rate limiter's Lua script against a real Redis (Compose's, on 6390).
test-redis:
	docker compose up --detach --wait redis
	GATEKEEPER_TEST_REDIS_URL=redis://127.0.0.1:6390/0 go test -race -count=1 -run Redis ./internal/ratelimit/

lint:
	@unformatted=$$(gofmt -l .); if [ -n "$$unformatted" ]; then \
		echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; fi
	go vet ./...

# --wait blocks until the container's health check reports healthy.
up:
	docker compose up --build --detach --wait

down:
	docker compose down

smoke: up
	./scripts/smoke.sh
	$(MAKE) down

clean:
	rm -rf bin
