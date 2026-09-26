GO ?= go
GOFMT ?= $(shell $(GO) env GOROOT)/bin/gofmt
export GOTOOLCHAIN := local

.PHONY: check toolchain deps fmt fmt-check vet test race build mod-check

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
