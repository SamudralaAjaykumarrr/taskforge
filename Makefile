.PHONY: build test test-race vet fmt-check up down test-chaos chaos-stress chaos-soak

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

# Phase 9 ("Chaos, Load, and Failure Testing", docs/roadmap.md):
# test-chaos is the CI-safe, bounded, deterministic-seed adversarial
# suite -- it is also already part of `test`/`test-race` above (it's an
# ordinary Go test package), this target just isolates it for quick,
# verbose reruns during chaos-harness development.
test-chaos:
	go test -p 1 -v ./internal/chaos/...

# chaos-stress and chaos-soak are cmd/chaos's manually invoked, heavier
# counterparts -- see cmd/chaos's package doc comment and README.md's
# "Phase 9: What's Implemented" for the full contract. TASKFORGE_DATABASE_URL
# (or TASKFORGE_TEST_DATABASE_URL) must point at a disposable/test
# database; SEED/JOBS/WORKFLOWS/WORKERS/DURATION override the defaults,
# e.g.: make chaos-stress SEED=42 DURATION=2m
SEED ?= 0
JOBS ?= 500
WORKFLOWS ?= 20
WORKERS ?= 25
DURATION ?= 60s

chaos-stress:
	go run ./cmd/chaos -mode=stress -seed=$(SEED) -jobs=$(JOBS) -workflows=$(WORKFLOWS) -workers=$(WORKERS) -duration=$(DURATION)

chaos-soak:
	go run ./cmd/chaos -mode=soak -seed=$(SEED) -jobs=$(JOBS) -workflows=$(WORKFLOWS) -workers=$(WORKERS) -duration=$(DURATION)
