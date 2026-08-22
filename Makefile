SHELL := /bin/sh
GO ?= go
COMPOSE ?= docker compose
GOLANGCI_LINT_VERSION ?= v2.12.2
# Must match the version pinned in .github/workflows/ci.yml (sqlc-drift job)
# and the header of the committed generated files; bump them together.
SQLC_VERSION ?= 1.31.1
# Must match the versions pinned in .github/workflows/ci.yml (ts-types-drift
# job); bump them together.
SUPABASE_VERSION ?= 2.114.0
MIGRATE_VERSION ?= v4.19.1
# Must match the version pinned in .github/workflows/ci.yml (openapi job);
# bump them together.
OPENAPI_VALIDATOR_VERSION ?= 2.46.1
DATABASE_URL_TEST ?= postgres://apivo:apivo@localhost:5432/apivo?sslmode=disable
# The race detector needs cgo; on Windows without a C toolchain, run
# `make test RACE=` and let CI cover the race detection.
RACE ?= -race

.PHONY: setup db-up db-down test test-unit cover vet lint openapi-lint sqlc ts-types web-install web-check web-build worker-test worker-validate hetzner-test hetzner-validate env-status

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

## openapi-lint: validate api/openapi.json against the OpenAPI specification (matches CI)
# Needs Node, not Docker: the same command the openapi CI job runs.
openapi-lint:
	npx --yes @redocly/cli@$(OPENAPI_VALIDATOR_VERSION) lint api/openapi.json

## sqlc: regenerate Go types from the schema migrations
sqlc:
	docker run --rm -v "$(CURDIR)":/src -w /src sqlc/sqlc:$(SQLC_VERSION) generate

## ts-types: regenerate TypeScript types from a freshly migrated scratch database
# The migrate container joins whatever network the compose project put
# postgres on, so the target works whatever the checkout directory is named.
ts-types: db-up
	$(COMPOSE) exec postgres psql -U apivo -d postgres -c "drop database if exists apivo_types with (force)" -c "create database apivo_types"
	docker run --rm -v "$(CURDIR)/internal/platform/db/migrations":/m --network "$$(docker inspect -f '{{range $$k, $$v := .NetworkSettings.Networks}}{{$$k}}{{end}}' "$$($(COMPOSE) ps -q postgres)")" migrate/migrate:$(MIGRATE_VERSION) -path /m -database "postgres://apivo:apivo@postgres:5432/apivo_types?sslmode=disable" up
	npx --yes supabase@$(SUPABASE_VERSION) gen types typescript --db-url "postgres://apivo:apivo@localhost:5432/apivo_types?sslmode=disable" > web/src/lib/database.types.ts

## web-install: install frontend dependencies exactly as locked
web-install:
	cd web && npm ci

## web-check: typecheck the frontend
web-check:
	cd web && npm run check

## web-build: production build of the frontend
web-build:
	cd web && npm run build

## worker-test: prove the Cloudflare Worker's routing rules (matches CI)
# Node's built-in test runner over the standard web objects - no dependency,
# no Cloudflare runtime. What only a real deploy can prove stays out of it.
worker-test:
	node --test 'deploy/cloudflare/*.test.mjs'

## worker-validate: full wrangler config parse and Worker bundle (matches CI)
# --dry-run needs no credentials and uploads nothing.
worker-validate:
	npx --yes wrangler@4 deploy --dry-run --containers-rollout none

## hetzner-test: prove the deployment reconciler's decisions (matches CI)
# A stub registry and daemon stand in for the real ones, so the rollback, the
# digest mismatch and the version mismatch are all exercised without a host.
hetzner-test:
	sh deploy/hetzner/bin/apivo-reconcile_test.sh
	sh deploy/hetzner/bin/apivo-previews_test.sh

## hetzner-validate: prove the whole VPS configuration without a VPS (matches CI)
# Every environment's compose configuration rendered and asserted, plus both
# Caddyfiles through `caddy validate`. This is to the Hetzner deployment what
# worker-validate is to the Cloudflare one.
hetzner-validate: hetzner-test
	shellcheck -s sh deploy/hetzner/bin/apivo-reconcile deploy/hetzner/bin/apivo-reconcile_test.sh deploy/hetzner/bin/apivo-previews deploy/hetzner/bin/apivo-previews_test.sh deploy/hetzner/bin/apivoctl deploy/hetzner/provision.sh deploy/hetzner/validate.sh scripts/env_status.sh
	sh deploy/hetzner/validate.sh

## env-status: what every environment is actually serving, right now
# One HTTPS request per environment. No credentials, no SSH, no VPS access.
env-status:
	sh scripts/env_status.sh
