# protoregistry — common dev tasks.
#
# Most targets are thin wrappers around `go` so they work the same in CI and
# in a local checkout. The exception is `test`, which needs Docker (it spins
# up PostgreSQL via testcontainers).

GO            ?= go
PKGS          ?= ./...
UNIT_PKGS     ?= ./snapshot/... ./compiler/... ./compat/... ./namespace/... ./resolve/... ./authz/...

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

# Regenerate sqlc and buf outputs. Requires `sqlc` and `buf` on PATH.
# Buf fetches the protoc-gen-go and protoc-gen-go-grpc plugins from
# buf.build at generation time, so contributors do not need them
# installed locally.
.PHONY: generate
generate: generate-sqlc generate-proto

.PHONY: generate-sqlc
generate-sqlc:
	sqlc generate

.PHONY: generate-proto
generate-proto:
	buf generate

# Lint the proto API against the STANDARD rule set defined in buf.yaml.
.PHONY: buf-lint
buf-lint:
	buf lint

# Check for breaking changes against the main branch. Useful in CI on PRs
# that touch proto. Override the base with BUF_BREAKING_AGAINST=...
BUF_BREAKING_AGAINST ?= .git#branch=main
.PHONY: buf-breaking
buf-breaking:
	buf breaking --against '$(BUF_BREAKING_AGAINST)'

# Canonicalize proto formatting. Mutates files in place; use buf-format-check
# in CI to verify without writing.
.PHONY: buf-format
buf-format:
	buf format -w

.PHONY: buf-format-check
buf-format-check:
	buf format --diff --exit-code

.PHONY: clean
clean:
	rm -f coverage.out
	rm -rf bin/ dist/
