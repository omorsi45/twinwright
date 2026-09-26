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

.PHONY: up
up: ## start PostgreSQL and an OpenTelemetry collector
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
	rm -f twinwright.manifest.json *.world.manifest.json bench-report.json
