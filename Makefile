.PHONY: build test lint up down smoke clean

build:
	go build -o bin/gatekeeper ./cmd/gatekeeper

# -race matters: the gateway handles every request on its own goroutine.
test:
	go test -race -count=1 ./...

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
