# protoregistry — common dev tasks.
#
# Most targets are thin wrappers around `go` so they work the same in CI and
# in a local checkout. The exception is `test`, which needs Docker (it spins
# up PostgreSQL via testcontainers).

GO            ?= go
PKGS          ?= ./...
UNIT_PKGS     ?= ./snapshot/... ./compiler/... ./compat/... ./namespace/... ./resolve/...

.PHONY: all
all: vet test-unit

.PHONY: build
build:
	$(GO) build $(PKGS)

.PHONY: vet
vet:
	$(GO) vet $(PKGS)

# Run golangci-lint with the config in .golangci.yml. Install via
# https://golangci-lint.run/welcome/install/ if you don't have it.
.PHONY: lint
lint:
	golangci-lint run $(PKGS)

.PHONY: test
test:
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic $(PKGS)

.PHONY: test-unit
test-unit:
	$(GO) test -race $(UNIT_PKGS)

.PHONY: headers
headers:
	./scripts/add-license-header.sh

.PHONY: headers-check
headers-check:
	./scripts/add-license-header.sh --check

# Regenerate sqlc and protoc outputs. Requires `sqlc`, `protoc`,
# `protoc-gen-go`, `protoc-gen-go-grpc` on PATH.
.PHONY: generate
generate: generate-sqlc generate-proto

.PHONY: generate-sqlc
generate-sqlc:
	sqlc generate

.PHONY: generate-proto
generate-proto:
	protoc --proto_path=proto \
		--go_out=proto --go_opt=paths=source_relative \
		--go-grpc_out=proto --go-grpc_opt=paths=source_relative \
		proto/protoregistry/v1/registry.proto

.PHONY: clean
clean:
	rm -f coverage.out
	rm -rf bin/ dist/
