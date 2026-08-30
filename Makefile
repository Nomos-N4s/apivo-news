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

.PHONY: setup db-up db-down test test-unit cover vet lint openapi-lint sqlc ts-types web-install web-check web-build worker-test worker-validate hetzner-test hetzner-validate env-status cashback-up cashback-seed cashback-scenario cashback-verify-ledger cashback-brand-check migration-lint ref-lint

# ---------------------------------------------------------------------------
# `missing` — how a cashback target behaves before its dependency has landed.
#
# The cashback quickstart (specs/002-apivo-cashback-alpha/quickstart.md) is a
# validation guide: every scenario in it is a command somebody is meant to run
# and believe. Several of those commands invoke work that other tasks own and
# that has not been merged yet.
#
# There are three ways to write a target in that position and only one of them
# is honest:
#
#   - do nothing and exit 0. The scenario "passes". This is the worst option
#     available, because the whole point of the guide is that a green result
#     means something.
#   - run the command anyway and let it fail. `go: cannot find package` or
#     `sh: scripts/lint-brand-literals.sh: No such file` tells the reader that
#     something is broken but not that it is UNBUILT - so the next hour goes
#     on a toolchain that was never at fault.
#   - fail deliberately, naming what is missing, which task and issue provides
#     it, and what to do in the meantime. That is this.
#
# Arguments must not contain commas - make splits $(call) arguments on them.
# They may contain `#`: the arguments are used inside double quotes, so the
# shell does not read one as a comment.
#
# THE EXIT LIVES INSIDE THE BRACES, and that is the whole correctness of this
# macro. Written as `{ ...; } >&2; exit 1` it expands, after a `||`, to:
#
#     test -f FILE || { printf ...; } >&2; exit 1
#
# which the shell reads as `(A || B); C`. The `exit 1` is a separate command
# in the list, so it runs whether or not the test passed - and every target
# below fails forever, including once its dependency has landed. Inside the
# braces the exit belongs to the right-hand side of the `||` and only runs
# when the guard actually fired.
#
# scripts/make_targets_test.sh exists because of this bug: the negative case
# alone could not catch it, since a target that always fails and a target that
# correctly reports a missing dependency are indistinguishable while the
# dependency is in fact missing.
# ---------------------------------------------------------------------------
missing = { printf '\n%s\n\n  missing:      %s\n  provided by:  %s\n  meanwhile:    %s\n\n%s\n\n' "make $@: a dependency of this target has not landed yet." "$(1)" "$(2)" "$(3)" "Failing on purpose. A target that quietly succeeded here would report a green result that nobody produced."; exit 1; } >&2

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
	sh deploy/hetzner/bin/apivo-seed-editors_test.sh

## hetzner-validate: prove the whole VPS configuration without a VPS (matches CI)
# Every environment's compose configuration rendered and asserted, plus both
# Caddyfiles through `caddy validate`. This is to the Hetzner deployment what
# worker-validate is to the Cloudflare one.
hetzner-validate: hetzner-test
	shellcheck -s sh deploy/hetzner/bin/apivo-reconcile deploy/hetzner/bin/apivo-reconcile_test.sh deploy/hetzner/bin/apivo-previews deploy/hetzner/bin/apivo-previews_test.sh deploy/hetzner/bin/apivoctl deploy/hetzner/bin/apivo-seed-editors deploy/hetzner/bin/apivo-seed-editors_test.sh deploy/hetzner/provision.sh deploy/hetzner/validate.sh scripts/env_status.sh
	sh deploy/hetzner/validate.sh

## env-status: what every environment is actually serving, right now
# One HTTPS request per environment. No credentials, no SSH, no VPS access.
env-status:
	sh scripts/env_status.sh

# ---------------------------------------------------------------------------
# Cashback (ADR-0002, ADR-0003)
#
# These are the commands specs/002-apivo-cashback-alpha/quickstart.md tells a
# reader to run. Several of them invoke work other tasks own; every one of
# those checks for its dependency first and fails with a message naming the
# task and the issue rather than with a toolchain error about a missing file.
# See the `missing` macro at the top of this file for why.
#
# None of them is a substitute for the invariant suite. A green scenario with
# a failing C-1..C-7 suite means nothing (constitution, principle IX).
# ---------------------------------------------------------------------------

# `NAME` and `ACCOUNT` are claimed here so that ONLY the command line can set
# them. Make lets an environment variable supply an undefined make variable,
# and `NAME` is already an environment variable on more machines than one
# would guess - on a WSL Ubuntu it is the HOSTNAME. Left unclaimed,
# `make cashback-scenario` with no argument silently ran
# `-run TestScenario/DESKTOP-F3RQOG4` instead of telling the reader that NAME
# was required. An unconditional assignment here is overridden by the command
# line and overrides the environment, which is exactly the precedence wanted.
NAME :=
ACCOUNT :=

## cashback-up: start Postgres, Redis and the Blnk ledger for local development
# The api migrates 0010-0017 when it boots and Blnk migrates its own schema on
# startup, so there is nothing to run between this and `go run ./cmd/apivo` -
# the same shape the deployed environments use, where the reconciler starts
# containers and each migrates itself.
#
# Without Docker, skip this entirely: LEDGER_DRIVER=memory runs the catalogue,
# click-out, entry state machine, wallet and payout orchestration with no
# containers at all. What it does not run is the Blnk conformance suite, the
# cross-schema zero-sum check, and every DATABASE_URL-keyed invariant test.
# Those are expected skips, not failures to chase.
cashback-up:
	@grep -q '^  blnk:' docker-compose.yml || \
		$(call missing,a blnk service in docker-compose.yml,task T002 (issue #149) - blnk and redis in the local compose stack,run the api with LEDGER_DRIVER=memory NETWORK_DRIVER=fixture and skip the ledger entirely)
	$(COMPOSE) up -d --wait postgres redis blnk

## cashback-seed: seed one fixture network, two merchants, three rate bands
# Pass ACCOUNT=<supabase-auth-user-id> to opt an account in as well. The
# account.id must equal the Supabase Auth user id, exactly as for news.
cashback-seed:
	@test -f cmd/apivo/seed_cashback.go || \
		$(call missing,cmd/apivo/seed_cashback.go,task T130 (issue #277) - the seed command behind this target,create a network and a merchant through the operator API by hand)
	DATABASE_URL="$(DATABASE_URL_TEST)" $(GO) run ./cmd/apivo seed cashback $(ACCOUNT)

## cashback-scenario: run one quickstart validation scenario end to end
# NAME is one of the quickstart's V1-V6 scenarios: earn-confirm,
# evidence-immutable, reversal, withdrawal-exactly-once, unattributed-and-held,
# reconciliation.
cashback-scenario:
	@test -n "$(NAME)" || { \
		printf '\n%s\n\n  %s\n\n' \
			"make cashback-scenario: NAME is required." \
			"NAME=earn-confirm | evidence-immutable | reversal | withdrawal-exactly-once | unattributed-and-held | reconciliation"; \
		exit 1; \
	} >&2
	@test -d internal/cashback/scenarios || \
		$(call missing,internal/cashback/scenarios/,task T129 (issue #276) - the quickstart scenarios as automated tests,follow the scenario by hand from quickstart.md section Validation scenarios)
	DATABASE_URL="$(DATABASE_URL_TEST)" $(GO) test -count=1 -v -run 'TestScenario/$(NAME)' ./internal/cashback/scenarios/

## cashback-verify-ledger: prove C-1 - every currency sums to zero, every wallet matches the ledger
# The check that carries the one invariant living outside our own schema
# (ADR-0002). It also runs continuously in a deployed environment, where it
# failing is an incident rather than a test result.
cashback-verify-ledger:
	@test -f internal/cashback/wallet/zerosum.go || \
		$(call missing,internal/cashback/wallet/zerosum.go,task T046 (issue #193) - the continuous zero-sum check,nothing - C-1 cannot be verified before the check that verifies it exists)
	DATABASE_URL="$(DATABASE_URL_TEST)" $(GO) test -count=1 -v -run 'TestZeroSum' ./internal/cashback/wallet/...

## cashback-brand-check: prove no product name, colour, domain or email is hardcoded
# Rebrandability is a test that goes red, not a claim in a document
# (constitution, Architecture Constraints / Rebrandability).
cashback-brand-check:
	@test -f scripts/lint-brand-literals.sh || \
		$(call missing,scripts/lint-brand-literals.sh,task T016 (issue #163) - the brand-literal lint,nothing - there is no other check for a hardcoded brand literal)
	sh scripts/lint-brand-literals.sh

## ref-lint: prove this branch's name may be merged
# A merged branch name is written verbatim into the merge commit, and
# Principle I forbids naming an assistant or a vendor there. The name is the
# last moment at which that is free to fix (constitution, Principle I).
#
# Judges the branch alone. The names already in history are judged by
# `sh scripts/lint-refs.sh --from-messages origin/main..HEAD`, which needs a
# range and so cannot be the thing a bare target runs on `main`.
ref-lint:
	@BRANCH=$$(git symbolic-ref --short -q HEAD) || \
		{ echo "ref-lint: detached HEAD - no branch name to judge"; exit 0; }; \
	sh scripts/lint-refs.sh "$$BRANCH"

## migration-lint: prove no foreign key crosses a product schema boundary
# A product domain may not reach into another product's schema, at any depth.
# The build fails on one rather than a review catching it (constitution,
# Architecture Constraints / Products).
migration-lint:
	@test -f scripts/lint-migrations.sh || \
		$(call missing,scripts/lint-migrations.sh,task T039 (issue #186) - the migration lint,read the migrations by hand - the arch test covers Go imports but nothing covers schema boundaries)
	sh scripts/lint-migrations.sh
