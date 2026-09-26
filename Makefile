GO ?= go
GOFMT ?= $(shell $(GO) env GOROOT)/bin/gofmt
export GOTOOLCHAIN := local

.PHONY: check toolchain deps fmt fmt-check vet test race build mod-check sqlc-generate

SQLC_VERSION := v1.31.1

check: toolchain fmt-check mod-check vet test race build

toolchain:
	@test "$$($(GO) env GOVERSION)" = "go$$(awk '/^go / { print $$2 }' backend/go.mod)" || \
		{ echo 'Use the exact Go version in backend/go.mod (automatic toolchain downloads disabled).'; exit 1; }
	@$(GO) version

deps:
	cd backend && $(GO) mod download

fmt:
	$(GOFMT) -w backend

fmt-check:
	@files="$$($(GOFMT) -l backend)" || exit $$?; test -z "$$files" || { printf '%s\n' "$$files"; exit 1; }

mod-check:
	cd backend && $(GO) mod tidy -diff
	cd backend && $(GO) mod verify

vet:
	cd backend && $(GO) vet -mod=readonly ./...

test:
	cd backend && $(GO) test -mod=readonly ./...

race:
	cd backend && $(GO) test -mod=readonly -race ./...

build:
	cd backend && $(GO) build -mod=readonly -trimpath -o ../.build/bin/ ./cmd/...

sqlc-generate:
	cd backend && $(GO) run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate -f sqlc.yaml

COMPOSE := docker compose --env-file infra/compose/.env -f infra/compose/compose.yaml
.PHONY: infra-init infra-config infra-up infra-status infra-down db-provision db-migrate db-seed infra-verify infra-restart-check auth-smoke corebanking-integration corebanking-smoke infra-reset

infra-init:
	python3 infra/compose/init-env.py

infra-config:
	$(COMPOSE) config --quiet

infra-up: infra-config
	$(COMPOSE) up -d --wait --wait-timeout 300

infra-status:
	$(COMPOSE) ps --all

infra-down:
	$(COMPOSE) down

db-provision:
	python3 infra/compose/database.py provision

db-migrate:
	python3 infra/compose/database.py migrate

db-seed:
	python3 infra/compose/database.py seed

infra-verify:
	python3 infra/compose/tests/verify.py

infra-restart-check:
	python3 infra/compose/tests/restart.py

auth-smoke: build
	python3 infra/compose/tests/auth_smoke.py

corebanking-integration:
	python3 infra/compose/tests/corebanking_integration.py

corebanking-smoke: build
	python3 infra/compose/tests/corebanking_smoke.py

infra-reset:
	@test "$(CONFIRM)" = "delete-gtb-data" || { echo 'Destructive reset requires CONFIRM=delete-gtb-data'; exit 1; }
	$(COMPOSE) down --volumes
