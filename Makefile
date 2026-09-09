.PHONY: build test test-race vet fmt-check up down test-chaos chaos-stress chaos-soak \
	vulncheck sbom release-build release-metadata checksums

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

# Phase 10 ("Supply-Chain & Release Hardening", docs/enterprise-roadmap.md):
# pinned tool versions used by the targets below, and by
# .github/workflows/ci.yml / scheduled-security.yml / release.yml, so that a
# local `make vulncheck`/`make sbom` run and the CI/release invocations use
# the exact same tool version -- never `@latest` in either place.
GOVULNCHECK_VERSION ?= v1.8.0
CYCLONEDX_GOMOD_VERSION ?= v1.12.0

# vulncheck runs the official Go vulnerability scanner (golang.org/x/vuln)
# against every package TaskForge actually builds. It is Go-native --
# `go run`, not a third-party GitHub Action -- so the exact same command
# runs identically here, in CI (.github/workflows/ci.yml), and on the
# scheduled scan (.github/workflows/scheduled-security.yml). A nonzero exit
# code (govulncheck exits 3 when a vulnerability is found) fails this
# target and, in CI, fails the build -- it is never piped through `|| true`.
# See docs/supply-chain-security.md.
vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X github.com/SamudralaAjaykumarrr/taskforge/internal/buildinfo.Version=$(VERSION) \
	-X github.com/SamudralaAjaykumarrr/taskforge/internal/buildinfo.Commit=$(COMMIT) \
	-X github.com/SamudralaAjaykumarrr/taskforge/internal/buildinfo.Date=$(DATE)
RELEASE_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64
RELEASE_BINS := api worker

# sbom generates one CycloneDX SBOM per released binary (cyclonedx-gomod's
# "app" subcommand, not "mod"): "mod" aggregates every module required by
# any package anywhere in the module and does not evaluate build
# constraints, which is not accurate for a specific compiled artifact.
# "app" evaluates build constraints the same way `go build` would for a
# given GOOS/GOARCH/CGO_ENABLED and a given main package, so it is run once
# per (binary, platform) pair -- the same matrix release-build compiles --
# with the exact same GOOS/GOARCH/CGO_ENABLED=0 env release-build uses, so
# each SBOM accurately reflects the binary it is later attested against
# (see release.yml's SBOM attestation steps). Never hand-edit a generated
# dist/*.sbom.cdx.json; regenerate it. See docs/supply-chain-security.md.
#
# cyclonedx-gomod is `go install`ed once into .tools/, deliberately not
# `go run` -- `go run` would recompile the tool itself under whatever
# GOOS/GOARCH is set in the loop below, so building the darwin/* SBOMs on a
# linux runner would cross-compile the *tool* for darwin and then fail to
# execute it locally ("exec format error"). The installed host binary is
# then invoked once per target with GOOS/GOARCH/CGO_ENABLED set only as
# environment input to its own build-constraint analysis, exactly as its
# own documentation describes. .tools/ is gitignored, ephemeral build
# tooling, not a project dependency.
SBOM_TOOL := $(CURDIR)/.tools/cyclonedx-gomod

sbom:
	mkdir -p dist .tools
	GOBIN=$(CURDIR)/.tools go install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@$(CYCLONEDX_GOMOD_VERSION)
	@for platform in $(RELEASE_PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		for bin in $(RELEASE_BINS); do \
			out=dist/taskforge-$$bin-$$os-$$arch.sbom.cdx.json; \
			echo "generating $$out"; \
			GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
				$(SBOM_TOOL) app -json -output $$out -licenses -main cmd/$$bin .; \
		done; \
	done

# release-build cross-compiles the api and worker binaries for every
# platform the release workflow publishes, embedding version metadata via
# -ldflags into internal/buildinfo (see that package's doc comment). This
# is the same build TaskForge's release workflow runs on a tag push
# (.github/workflows/release.yml) -- run it locally to exercise the release
# build without publishing anything. VERSION defaults to a dev/dirty
# marker derived from git when not overridden.
#
# dist/ is removed and recreated first: release-build is the first step of
# the release pipeline (clean -> binaries -> SBOMs -> release metadata ->
# checksums -> attest -> publish), so a stale binary/SBOM left over from an
# earlier local build must not survive into this build's output, and
# therefore must not end up in SHA256SUMS or a published GitHub Release.
release-build:
	rm -rf dist
	mkdir -p dist
	@for platform in $(RELEASE_PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		for bin in $(RELEASE_BINS); do \
			out=dist/taskforge-$$bin-$$os-$$arch; \
			echo "building $$out"; \
			GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" \
				-o $$out ./cmd/$$bin; \
		done; \
	done

# release-metadata writes one small, deterministic, machine-readable
# manifest describing the release as a whole (version/tag, commit, the Go
# toolchain version release-build actually compiled with, build date, and
# the exact supported target OS/arch set) -- a supplement to the
# per-binary version metadata already embedded via internal/buildinfo and
# readable via `-version`, not a replacement for it. It is included in
# SHA256SUMS and published as a release asset like every other file in
# dist/, but it is not itself cryptographic proof of anything -- see
# docs/supply-chain-security.md.
release-metadata:
	mkdir -p dist
	@go_version="$$(go env GOVERSION)"; \
	printf '{\n  "version": "%s",\n  "commit": "%s",\n  "date": "%s",\n  "go_version": "%s",\n  "targets": [%s],\n  "binaries": [%s]\n}\n' \
		"$(VERSION)" "$(COMMIT)" "$(DATE)" "$$go_version" \
		"$$(printf '"%s", ' $(RELEASE_PLATFORMS) | sed 's/, $$//')" \
		"$$(printf '"%s", ' $(RELEASE_BINS) | sed 's/, $$//')" \
		> dist/release-metadata.json
	cat dist/release-metadata.json

# checksums produces a SHA-256 manifest over every file currently in dist/
# (release binaries, their SBOMs, and release-metadata.json), in the
# conventional `sha256sum`-compatible format `gh attestation verify` /
# `sha256sum -c` both understand. Must run after every other dist/-writing
# target (sbom, release-metadata) so nothing is missing from the manifest,
# and release-build's clean step (above) is what guarantees nothing stale
# is present either.
checksums:
	cd dist && rm -f SHA256SUMS && sha256sum * > SHA256SUMS
