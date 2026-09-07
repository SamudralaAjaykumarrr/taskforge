.PHONY: build test test-race vet fmt-check up down

build:
	go build ./...

vet:
	go vet ./...

fmt-check:
	@fmt_out="$$(gofmt -l .)"; \
	if [ -n "$$fmt_out" ]; then \
		echo "gofmt needed on:"; echo "$$fmt_out"; exit 1; \
	fi

# -p 1 is required, not just a preference: integration tests in different
# packages share one PostgreSQL instance (see internal/testutil), so their
# test binaries must not run concurrently. See docs/testing-strategy.md.
test:
	go test -p 1 ./...

test-race:
	go test -race -p 1 ./...

up:
	docker compose up -d

down:
	docker compose down
