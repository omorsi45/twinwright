# Twinwright developer entry points.
#
# Everything here is a thin wrapper over the `go` and `docker compose` commands
# it runs, so nothing is hidden: `make test` is `go test ./...`. The targets
# exist so the same commands run locally and in CI, not to invent a build
# system.

SHELL := /bin/bash
.DEFAULT_GOAL := help

POSTGRES_DSN ?= postgres://twinwright:twinwright@127.0.0.1:5432/twinwright?sslmode=disable

.PHONY: help
help: ## show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## build the CLI into ./twinwright
	go build -o twinwright ./cmd/twinwright

.PHONY: demo
demo: build ## run the 12-step scripted demo in a temporary directory (no network, no keys)
	./twinwright demo --dir "$$(mktemp -d)/demo"

.PHONY: demo-stack
demo-stack: ## start the Compose stack and run the fleet, takeover and observability demo against it
	./scripts/demo-stack.sh

.PHONY: fmt
fmt: ## format all Go source
	gofmt -w .

.PHONY: check
check: ## gofmt check, vet and build, the way CI runs them
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "not gofmt-clean:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	go build ./...

.PHONY: test
test: ## run the hermetic test suite (SQLite only, no network)
	go test -count=1 ./...

.PHONY: test-race
test-race: ## run the suite under the race detector
	go test -race -count=1 ./...

.PHONY: test-postgres
test-postgres: ## run the suite with PostgreSQL integration tests enabled
	TWINWRIGHT_TEST_POSTGRES_DSN="$(POSTGRES_DSN)" go test -count=1 ./...

.PHONY: e2e
e2e: ## deterministic end-to-end walk of the documented pipeline (SQLite)
	./scripts/e2e.sh

.PHONY: e2e-postgres
e2e-postgres: ## end-to-end walk against PostgreSQL
	./scripts/e2e.sh "$(POSTGRES_DSN)"

.PHONY: bench
bench: ## measure runtime primitives on SQLite (see docs/performance.md)
	./scripts/perf.sh

.PHONY: bench-postgres
bench-postgres: ## measure runtime primitives on SQLite and PostgreSQL
	./scripts/perf.sh "$(POSTGRES_DSN)"

.PHONY: bench-smoke
bench-smoke: ## run every benchmark once, to prove they still compile and pass
	go test -run '^$$' -bench . -benchtime=1x -timeout 900s ./internal/store ./internal/worker ./internal/replay

.PHONY: perf
perf: ## measure the platform workloads on SQLite into perf.json (results are documented in docs/performance.md)
	go run ./cmd/twinwright perf --out perf.json

.PHONY: gate
gate: build ## bench the scripted suite and gate it against the approved baseline
	dir="$$(mktemp -d)" && trap 'rm -rf "$$dir"' EXIT && \
	  ./twinwright bench --suite standard --agent scripted --examples examples --out "$$dir/candidate.json" > /dev/null && \
	  ./twinwright gate --baseline examples/bench/baselines/standard-scripted.json --candidate "$$dir/candidate.json"

.PHONY: live-model
live-model: ## run a local Ollama model on the billing world and replay it (needs TW_LIVE_MODEL; LIVE_BASE_URL optional)
	./scripts/live-model.sh "$(LIVE_BASE_URL)" "$(TW_LIVE_MODEL)"

.PHONY: up
up: ## start PostgreSQL, the OpenTelemetry collector, Jaeger and Prometheus
	docker compose up -d
	@echo "waiting for postgres to accept connections"
	@until docker compose exec -T postgres pg_isready -U twinwright >/dev/null 2>&1; do sleep 1; done
	@echo "postgres is ready at $(POSTGRES_DSN)"

.PHONY: down
down: ## stop and remove the local services and their data
	docker compose down -v

.PHONY: verify
verify: check test e2e ## everything the fast CI lane runs

.PHONY: verify-full
verify-full: check test-race test-postgres e2e e2e-postgres ## everything, including PostgreSQL (needs `make up`)

.PHONY: clean
clean: ## remove build output and local databases
	rm -f twinwright twinwright.exe
	rm -f twinwright*.db twinwright*.db-wal twinwright*.db-shm
	rm -f twinwright.manifest.json *.world.manifest.json bench-report.json bench-candidate.json perf.json
