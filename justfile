set shell := ["bash", "-euo", "pipefail", "-c"]

# List commands.
default:
    @just --list

fmt:
    gofmt -w cmd internal

fmt-check:
    @files="$(gofmt -l cmd internal)"; if [ -n "$files" ]; then echo "$files"; exit 1; fi

test:
    mkdir -p .coverage
    go test -race -count=1 -covermode=atomic -coverprofile=.coverage/coverage.out ./...
    go tool cover -func=.coverage/coverage.out | tail -1
    go tool cover -func=.coverage/coverage.out | awk '/^total:/ { sub(/%$/, "", $3); if ($3 < 90) exit 1 }'

lint:
    go vet ./...
    go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...

vuln:
    go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

check: fmt-check lint test test-pure vuln test-artifacts

# Check Actions expressions, permissions and reusable-workflow interfaces.
lint-actions:
    go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12 .github/workflows/*.yml

container-smoke image="roi:local":
    sh scripts/container-smoke.sh {{quote(image)}}

build:
    sh scripts/build.sh bin/roi

run config="examples/config.yaml":
    go run ./cmd/roi --config {{quote(config)}}

password-hash:
    go run ./cmd/roi password-hash

# Verify application tests without the C toolchain (race detector itself needs CGO).
test-pure:
    CGO_ENABLED=0 go test -count=1 ./...

# Write an HTML coverage report after running the race-enabled test suite.
coverage: test
    go tool cover -html=.coverage/coverage.out -o .coverage/index.html

# Bounded parser fuzzing; override the duration, e.g. just fuzz 1m.
fuzz duration="15s":
    go test ./internal/roi -run '^$' -fuzz '^FuzzItemIDRoundTrip$' -fuzztime={{quote(duration)}} -parallel=4
    go test ./internal/roi -run '^$' -fuzz '^FuzzSubscriptionDocuments$' -fuzztime={{quote(duration)}} -parallel=4
    go test ./internal/roi -run '^$' -fuzz '^FuzzGitwebDocuments$' -fuzztime={{quote(duration)}} -parallel=4

benchmark:
    go test ./internal/roi -run '^$' -bench '^BenchmarkLargeArchiveMetadata$' -benchmem

# Offline maintenance. Makes a verified backup before reclaiming free pages.
compact config="examples/config.yaml":
    go run ./cmd/roi --config {{quote(config)}} compact

# Install Playwright separately, as documented in README.md.
browser: build
    node tests/browser.cjs

# Python is only needed for artifact tooling, not the roi runtime.
test-artifacts:
    python3 -m unittest discover -s tests -p 'test_*.py'

artifacts: build test-artifacts
    python3 scripts/artifacts.py bin/roi
