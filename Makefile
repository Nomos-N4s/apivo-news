SHELL := /bin/sh
GO ?= go
COMPOSE ?= docker compose
GOLANGCI_LINT_VERSION ?= v2.12.2
# Must match the version pinned in .github/workflows/ci.yml (sqlc-drift job)
# and the header of the committed generated files; bump them together.
SQLC_VERSION ?= 1.31.1
DATABASE_URL_TEST ?= postgres://apivo:apivo@localhost:5432/apivo?sslmode=disable
# The race detector needs cgo; on Windows without a C toolchain, run
# `make test RACE=` and let CI cover the race detection.
RACE ?= -race

.PHONY: setup db-up db-down test test-unit cover vet lint sqlc web-install web-check web-build

## setup: one-time developer setup - route git hooks through .githooks
setup:
	git config core.hooksPath .githooks

## db-up: start the local Postgres and wait until it is healthy
db-up:
	$(COMPOSE) up -d --wait postgres

## db-down: stop the local stack
db-down:
	$(COMPOSE) down

## test-unit: run tests that need no database (schema invariant tests skip)
test-unit:
	$(GO) test $(RACE) -shuffle=on ./...

## test: run the full suite, including schema invariant tests, against compose Postgres
test: db-up
	DATABASE_URL="$(DATABASE_URL_TEST)" $(GO) test $(RACE) -shuffle=on ./...

## cover: full suite with the CI coverage gate applied
cover: db-up
	DATABASE_URL="$(DATABASE_URL_TEST)" $(GO) test $(RACE) -shuffle=on -covermode=atomic -coverprofile=coverage.out -coverpkg=./... ./...
	sh scripts/coverage_gate.sh 90 coverage.out

## vet: run go vet
vet:
	$(GO) vet ./...

## lint: run golangci-lint in a container (matches CI)
lint:
	docker run --rm -v "$(CURDIR)":/src -w /src golangci/golangci-lint:$(GOLANGCI_LINT_VERSION) golangci-lint run

## sqlc: regenerate Go types from the schema migrations
sqlc:
	docker run --rm -v "$(CURDIR)":/src -w /src sqlc/sqlc:$(SQLC_VERSION) generate

## web-install: install frontend dependencies exactly as locked
web-install:
	cd web && npm ci

## web-check: typecheck the frontend
web-check:
	cd web && npm run check

## web-build: production build of the frontend
web-build:
	cd web && npm run build
