SHELL := /bin/sh
GO ?= go
NODE ?= node
PNPM ?= pnpm
NODE_BIN_DIR = $(shell "$(NODE)" -p 'require("node:path").dirname(process.execPath)')
CONFIG ?= $(CURDIR)/config/development.example.json
SQLC ?= $(CURDIR)/.runtime/bin/sqlc
PG_TEST_BIN_DIR ?= $(CURDIR)/.runtime/postgresql-17.11/bin

.PHONY: help check-node test-unit test-go vet-go test-device test-editor test-contract \
	generate generate-api generate-sql install-tools check-generated typecheck-device typecheck-editor test-browser test-browser-editor test-db test-db-integration \
	build-go build-web dev-api dev-web test-admin-flow build-admin

.PHONY: test-device-flow build migrate dev-up dev-workers test-e2e smoke

# Aggregate production build of every deployable artifact.
build: build-go build-admin build-web
	@echo 'build: api + admin binaries and frontend dist ready'

# Single ordered migration run against the development DSN in
# .runtime/development/postgres.dsn (set up per README 管理员同源 HTTPS 开发).
migrate:
	@dsn="$$(cat .runtime/development/postgres.dsn 2>/dev/null)" || { echo 'migrate: .runtime/development/postgres.dsn missing (see README)'; exit 2; }; \
	psql "$$dsn" -v ON_ERROR_STOP=1 -f backend/db/migrations/001_identity.sql >/dev/null && \
	psql "$$dsn" -v ON_ERROR_STOP=1 -f backend/db/migrations/002_device_session.sql >/dev/null && \
	psql "$$dsn" -v ON_ERROR_STOP=1 -f backend/db/migrations/003_mfa.sql >/dev/null && \
	psql "$$dsn" -v ON_ERROR_STOP=1 -f backend/db/migrations/004_credit.sql >/dev/null && \
	psql "$$dsn" -v ON_ERROR_STOP=1 -f backend/db/migrations/005_document.sql >/dev/null && \
	psql "$$dsn" -v ON_ERROR_STOP=1 -f backend/db/migrations/006_generation.sql >/dev/null && \
	psql "$$dsn" -v ON_ERROR_STOP=1 -f backend/db/migrations/007_capacity.sql >/dev/null && \
	psql "$$dsn" -v ON_ERROR_STOP=1 -f backend/db/migrations/008_artifact.sql >/dev/null && \
	psql "$$dsn" -v ON_ERROR_STOP=1 -f backend/db/migrations/009_archive_audit.sql >/dev/null && \
	psql "$$dsn" -v ON_ERROR_STOP=1 -f backend/db/migrations/010_auth_limits.sql >/dev/null && \
	psql "$$dsn" -v ON_ERROR_STOP=1 -f backend/db/migrations/011_render_profile.sql >/dev/null && \
	psql "$$dsn" -v ON_ERROR_STOP=1 -f backend/db/migrations/012_preview.sql >/dev/null && \
	psql "$$dsn" -v ON_ERROR_STOP=1 -f backend/db/migrations/013_generation_store.sql >/dev/null && \
	echo 'migrate: 001-013 applied in order'

# Local infrastructure readiness check. This deployment runs PostgreSQL, the
# API and both workers as one supervised process; dev-up verifies and starts
# that process against the development configuration.
dev-up: build
	@test -f .runtime/development/postgres.dsn || { echo 'dev-up: prepare .runtime/development/postgres.dsn first (README)'; exit 2; }; \
	test -x .runtime/bin/api || { echo 'dev-up: api binary missing'; exit 2; }; \
	echo 'dev-up: development database, API, preview/formal workers and finalizer run inside .runtime/bin/api (see README dev-api)'

# The preview and formal workers plus the delivery finalizer are in-process
# loops of the API binary; this target documents and verifies that wiring.
dev-workers:
	@test -x .runtime/bin/api && echo 'dev-workers: preview queue, formal queue and finalizer are started with the API process' || { echo 'run make build first'; exit 2; }

# Browser end-to-end against a real temporary PostgreSQL cluster and Chrome.
test-e2e: test-admin-flow test-device-flow

# Post-deployment role/billing smoke against a running development origin.
smoke:
	@test -n "$(origin)" || { echo 'usage: make smoke origin=https://127.0.0.1:PORT'; exit 2; }; \
	curl -fsS "$(origin)/healthz" >/dev/null && echo 'smoke: healthz ok'; \
	code=$$(curl -s -o /dev/null -w '%{http_code}' "$(origin)/api/v1/me"); \
	test "$$code" = "401" && echo 'smoke: business endpoints authenticated (401 without session)' || { echo "smoke: unexpected /me status $$code"; exit 1; }

help:
	@echo 'Available: test-unit test-go vet-go test-device test-editor test-contract'
	@echo '           generate check-generated typecheck-device typecheck-editor test-browser test-db test-db-integration'
	@echo '           build-go build-web dev-api dev-web'
	@echo '           build-admin test-admin-flow (private PG17 + built React + Chrome)'
	@echo '           test-device-flow (initial USER authorization, restart and attack cases)'
	@echo 'See README.md for scope, dependencies and incomplete integration gates.'

check-node:
	@"$(NODE)" -e 'if (process.versions.node.split(".")[0] !== "24") { throw new Error("Node 24 is required"); }'

test-unit: test-go test-device test-editor test-contract

test-go:
	cd backend && "$(GO)" test ./... -count=1

vet-go:
	cd backend && "$(GO)" vet ./...

test-device: check-node
	cd frontend && "$(NODE)" --test tests/device/*.test.mjs tests/api/*.test.mjs

test-editor: check-node
	cd frontend && "$(NODE)" --test tests/editor/*.test.mjs

test-contract: check-node
	"$(NODE)" --test scripts/*.test.mjs

generate: generate-sql generate-api

install-tools:
	mkdir -p .runtime/bin
	cd backend && GOBIN="$(CURDIR)/.runtime/bin" "$(GO)" install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
	cd backend && GOBIN="$(CURDIR)/.runtime/bin" "$(GO)" install golang.org/x/vuln/cmd/govulncheck@v1.9.0

generate-sql:
	"$(SQLC)" generate

generate-api: check-node
	"$(NODE)" scripts/generate-api-types.mjs

check-generated: check-node
	"$(NODE)" scripts/generate-api-types.mjs --check

# TSC_JS may select an already installed TypeScript 5.9.3 compiler without
# installing packages. Normal development uses the frontend local dependency.
define typecheck_project
	@if [ -n "$(TSC_JS)" ]; then \
		"$(NODE)" "$(TSC_JS)" --project "$(1)" --noEmit; \
	else \
		cd frontend && PATH="$(NODE_BIN_DIR):$$PATH" "$(PNPM)" exec tsc --project "$(CURDIR)/$(1)" --noEmit; \
	fi
endef

typecheck-device: check-node
	$(call typecheck_project,frontend/tsconfig.device.json)

typecheck-editor: check-node
	$(call typecheck_project,frontend/tsconfig.editor.json)

test-browser: check-node
	"$(NODE)" frontend/tests/device/browser-persistence.mjs

test-browser-editor: test-admin-flow

test-admin-flow: build-web
	PG_TEST_BIN_DIR="$(PG_TEST_BIN_DIR)" GO_BIN="$(GO)" NODE_BIN="$(NODE)" bash scripts/test-db.sh --admin-flow

test-device-flow: build-web
	PG_TEST_BIN_DIR="$(PG_TEST_BIN_DIR)" GO_BIN="$(GO)" NODE_BIN="$(NODE)" bash scripts/test-db.sh --device-flow

test-db:
	PG_TEST_BIN_DIR="$(PG_TEST_BIN_DIR)" bash scripts/test-db.sh

test-db-integration:
	PG_TEST_BIN_DIR="$(PG_TEST_BIN_DIR)" GO_BIN="$(GO)" bash scripts/test-db.sh --integration

build-go:
	mkdir -p .runtime/bin
	cd backend && "$(GO)" build -trimpath -o ../.runtime/bin/api ./cmd/api

build-admin:
	mkdir -p .runtime/bin
	cd backend && "$(GO)" build -trimpath -o ../.runtime/bin/admin ./cmd/admin

build-web: check-node
	cd frontend && PATH="$(NODE_BIN_DIR):$$PATH" "$(PNPM)" run build

dev-api:
	cd backend && "$(GO)" run ./cmd/api -config "$(CONFIG)"

dev-web: check-node
	cd frontend && PATH="$(NODE_BIN_DIR):$$PATH" "$(PNPM)" run dev
