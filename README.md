# protoregistry

A multi-namespace protobuf schema registry for Go, with versioning, staging, backward compatibility enforcement, and hot-swap capabilities.

> **Status:** `v0.x` pre-stable. The gRPC service in `proto/protoregistry/v1/` is the durable integration point; the Go library API may change at minor versions until `v1.0`. See [`STABILITY.md`](STABILITY.md) for the full contract.

Protoregistry compiles `.proto` files at runtime using [protocompile](https://github.com/bufbuild/protocompile), stores versioned schemas in PostgreSQL with content-addressable deduplication, and serves compiled descriptors for dynamic message creation and validation via gRPC.

## Features

- **Multi-namespace isolation** — each namespace is a self-contained scope (chroot model); proto imports resolve only within the same namespace
- **Two-phase staging** — publish compiles and stages; promote atomically swaps all staged versions to current, enabling coordinated multi-schema changes
- **Backward compatibility enforcement** — breaking changes (field deletion, type changes, cardinality changes) are rejected at promote time
- **Content-addressable storage** — proto sources are normalized, SHA-256 hashed, and deduplicated; rollback is a pointer move with zero data duplication
- **Hot-swap** — readers access compiled descriptors via `atomic.Pointer`; swaps are instant and lock-free
- **Dynamic message support** — create `dynamicpb.Message` instances from any registered schema at runtime
- **Custom built-in types** — extend the standard Google well-known types with your own shared protos via the reserved `__builtins__` namespace
- **Well-known type shadowing protection** — publishing files that shadow Google well-known types is rejected by default; requires explicit `force` flag
- **Startup recovery** — rebuilds in-memory state from pre-compiled descriptors in Postgres without recompilation
- **CLI tool** — `protoregistry` binary for managing the registry and running the gRPC server
- **Go client SDK** — `protoregistry/client` provides a remote-backed `protoreflect.MessageTypeResolver` / `protodesc.Resolver` with eager population, polling refresh, version pinning, and atomic hot-swap (see [Go client SDK](#go-client-sdk))

## Quick Start

### Running the server

```bash
# Build the binary
go build -o protoregistry ./cmd/protoregistry/

# Start the server (runs migrations and listens on :50051)
protoregistry serve --db "postgres://localhost:5432/protoregistry?sslmode=disable" --migrate --listen :50051

# Optionally bootstrap built-in types from a directory
protoregistry serve --db "$DATABASE_URL" --migrate --builtins ./company-types/
```

### Using the CLI

```bash
# Create a namespace
protoregistry namespace create acme

# Push proto files (publish + stage)
protoregistry push acme billing ./protos/billing/

# Promote staged changes to current
protoregistry promote acme

# Load an entire directory of proto files in dependency order
protoregistry load acme ./protos/ --promote

# List namespaces and schemas
protoregistry namespace list
protoregistry schema list acme
protoregistry schema info acme billing

# Retrieve source or compiled descriptors
protoregistry schema source acme billing --version 2
protoregistry schema descriptor acme billing --out billing.binpb

# Rollback to a previous version
protoregistry rollback acme billing 1 --promote

# Discard all staged changes
protoregistry discard acme
```

### Using as a Go library

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/jackc/pgx/v5/pgxpool"

    protoregistry "github.com/trendvidia/protoregistry"
    "github.com/trendvidia/protoregistry/store/postgres"
)

func main() {
    ctx := context.Background()

    pool, err := pgxpool.New(ctx, "postgres://localhost:5432/protoregistry?sslmode=disable")
    if err != nil {
        log.Fatal(err)
    }
    defer pool.Close()

    store := postgres.New(pool)
    reg := protoregistry.New(store)

    if err := reg.Restore(ctx); err != nil {
        log.Fatal(err)
    }

    result, err := reg.Publish(ctx, &protoregistry.PublishRequest{
        NamespaceID: "acme",
        SchemaID:    "billing",
        Sources: map[string][]byte{
            "billing/config.proto": []byte(`
                syntax = "proto3";
                package billing;
                message Config {
                    string name = 1;
                    int32 timeout_ms = 2;
                }
            `),
        },
        CreatedBy: "deploy-bot",
    })
    if err != nil {
        log.Fatal(err)
    }
    fmt.Printf("Published version %d (no_change=%v)\n", result.Version, result.NoChange)

    promoted, err := reg.Promote(ctx, "acme")
    if err != nil {
        log.Fatal(err)
    }
    fmt.Printf("Promoted %d schema(s)\n", len(promoted.Promoted))

    snap := reg.Current("acme", "billing")
    msg, _ := snap.NewMessage("billing.Config")
    fmt.Printf("Created dynamic message: %s\n", msg.ProtoReflect().Descriptor().FullName())
}
```

## CLI Reference

### Global flags

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--server, -s` | `PROTOREGISTRY_SERVER` | `localhost:50051` | gRPC server address |
| `--namespace, -n` | | | Default namespace for commands |
| `--output, -o` | | `table` | Output format: `table` or `json` |
| `--token` | `PROTOREGISTRY_TOKEN` | | Bearer token for authentication |
| `--tls` | | `false` | Connect over TLS using the system root CA pool |
| `--tls-ca` | | | PEM-encoded CA file to verify the server cert (implies `--tls`) |
| `--tls-cert` | | | PEM-encoded client certificate for mTLS (implies `--tls`) |
| `--tls-key` | | | PEM-encoded client key for mTLS (implies `--tls`) |
| `--tls-server-name` | | | Override the server name used for cert verification (implies `--tls`) |
| `--tls-skip-verify` | | `false` | Skip server cert verification — testing only (implies `--tls`) |

### Commands

| Command | Description |
|---------|-------------|
| `serve` | Start the gRPC registry server |
| `namespace list` | List all namespaces |
| `namespace create <id>` | Create a namespace |
| `schema list [namespace]` | List schemas in a namespace |
| `schema info [namespace] <schema>` | Show schema details |
| `schema source [namespace] <schema>` | Show proto source files |
| `schema descriptor [namespace] <schema>` | Get compiled FileDescriptorSet |
| `push [namespace] <schema> <path...>` | Publish proto files as a schema version |
| `load [namespace] <path>` | Load all protos from a directory in dependency order |
| `promote [namespace]` | Promote all staged versions to current |
| `discard [namespace]` | Discard all staged versions |
| `rollback [namespace] <schema> <version>` | Stage a previous version |

### `serve` flags

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--db` | `DATABASE_URL` | | PostgreSQL connection URL (required) |
| `--listen` | | `:50051` | gRPC listen address |
| `--builtins` | | | Directory of built-in `.proto` files to bootstrap |
| `--migrate` | | `false` | Run database migrations on startup |

### `push` / `load` flags

| Flag | Default | Description |
|------|---------|-------------|
| `--created-by` | `$USER` | Author of this version |
| `--promote` | `false` | Promote immediately after publishing |
| `--force` | `false` | Allow shadowing well-known types |
| `--metadata` | | Key=value metadata pairs (`push` only) |

## Staging Workflow

Schema updates follow a two-phase model, similar to git staging:

```
1. Publish    ->  compile + store + stage
2. Promote    ->  compat check + atomic swap (all staged -> current)
```

Multiple schemas can be staged independently, then promoted together as a coordinated set. The compiler resolves imports against the "proposed" state (staged where available, current otherwise), so cross-schema changes compile against each other before going live.

```
Developer pushes "common" v3 to staging
Developer pushes "billing" v5 to staging  (compiles against common v3)
Developer promotes                         ->  both go live atomically
```

Rollback stages a previous version, then promotes it:

```bash
protoregistry rollback acme billing 1    # stages v1
protoregistry promote acme               # v1 becomes current
```

## Built-in Types

The compiler provides Google well-known types (`google/protobuf/timestamp.proto`, etc.) automatically via protocompile. To add your own shared types available to all namespaces, publish them to the reserved `__builtins__` namespace:

```bash
# Push company-wide shared types as built-ins
protoregistry push __builtins__ company-types ./protos/company/
protoregistry promote __builtins__

# Now any namespace can import them:
#   import "company/base.proto";
```

The import resolution order during compilation is:

1. **Namespace sources** — the schema's own files + other schemas in the same namespace
2. **Built-ins** — files from the `__builtins__` namespace
3. **Google well-known types** — `google/protobuf/*.proto` (provided by protocompile)

### Well-known type protection

Publishing a file that shadows a Google well-known type (e.g.,
`google/protobuf/timestamp.proto`) is **rejected by default**. The check
exists because shadowing happens silently — the protocompile resolver picks
the namespace-local file before falling back to standard imports, so a
typo'd or malicious filename can replace the well-known type for every
schema in the namespace and break compilation in confusing ways down the
line.

When you genuinely need to substitute a well-known type (for example, to
provide a richer `Timestamp` with extra fields), pass `--force`:

```bash
protoregistry push __builtins__ custom-timestamp ./my-timestamp/ --force
```

This flag is intended for operator use; it should not be exposed to
self-service publishers.

The server can also bootstrap built-ins from a directory on disk at startup:

```bash
protoregistry serve --db "$DATABASE_URL" --builtins ./company-types/
```

## Architecture

```
.proto source -> protocompile -> compiled descriptors
                                      |
                            +-------------------+
                            |     Registry      |
                            |  (orchestrator)   |
                            +--------+----------+
                     +---------------+---------------+
                     v               v               v
              +-------------+ +------------+ +--------------+
              |  Namespace  | |   Store    | |    Compat    |
              | (in-memory) | | (Postgres) | |  (checker)   |
              +-------------+ +------------+ +--------------+
                     v
              +-------------+
              |  Snapshot   |  <- atomic.Pointer, lock-free reads
              | (immutable) |
              +-------------+
                     v
              +-------------+
              |  Resolver   |  <- protobuf-go bridge
              | (dynamicpb) |
              +-------------+
```

## Database

Protoregistry uses PostgreSQL with [sqlc](https://sqlc.dev/) for type-safe queries and [goose](https://github.com/pressly/goose) for migrations.

```bash
# Run migrations
goose -dir migrations postgres "$DATABASE_URL" up
```

Storage uses a content-addressable design with a versioning indirection layer:

```
proto_blobs (namespace_id, sha256) -> original source
     ^
schema_version_files (version, filename) -> blob_sha256
     ^
schema_versions (version) -> compiled FileDescriptorSet + compiler_version
     ^
schemas (namespace_id, schema_id) -> current_version / staged_version
```

Same content submitted multiple times (or across tenants) is stored once. Rollback is a pointer move — no data is copied.

## gRPC API

The `RegistryService` exposes the full lifecycle over gRPC:

| RPC | Description |
|-----|-------------|
| `Publish` | Compile and stage a new schema version |
| `Promote` | Atomically move all staged versions to current |
| `DiscardStaging` | Clear all staged versions in a namespace |
| `Rollback` | Stage a previous version for promotion |
| `GetSchema` | Get schema metadata and version list |
| `ListSchemas` | List all schemas in a namespace |
| `GetDescriptor` | Get compiled `FileDescriptorSet` for a version |
| `GetSource` | Get original `.proto` source files for a version |
| `ListNamespaces` | List all namespaces |
| `CreateNamespace` | Create a new namespace |

See [`proto/protoregistry/v1/registry.proto`](proto/protoregistry/v1/registry.proto) for the full definition.

## Type Resolution

The `resolve` package bridges namespace snapshots with protobuf-go's standard resolver interfaces:

```go
import "github.com/trendvidia/protoregistry/resolve"

// Namespace-wide resolver — searches all schemas.
r := resolve.NewResolver(namespace)
mt, _ := r.FindMessageByName("billing.Config")
msg := dynamicpb.NewMessage(mt.Descriptor())

// Schema-scoped resolver.
sr := resolve.NewSchemaResolver(namespace, "billing")
msg, _ := sr.NewMessage("billing.Config")
```

Resolvers are live — they always read the current snapshot, so hot-swaps are immediately reflected.

## Go client SDK

[`github.com/trendvidia/protoregistry/client`](client/) is the Go SDK for services that *consume* descriptors from a running registry, as opposed to embedding the registry library in-process. The client is namespace-scoped and implements the same standard resolver interfaces (`protoreflect.MessageTypeResolver`, `protoregistry.ExtensionTypeResolver`, `protodesc.Resolver`) as the in-process `resolve` package, so call sites that read descriptors don't change when you swap embedded for remote.

```go
import (
    "context"

    "github.com/trendvidia/protoregistry/client"
)

ctx := context.Background()
r, err := client.Dial(ctx, "registry.internal:50051", "billing")
if err != nil { /* ... */ }
defer r.Close()

desc, _ := r.FindDescriptorByName("billing.Config")
msg, _  := r.NewMessage("billing.Config")
```

Behavior:

- **Eager population.** `Dial` / `client.New` fetches every schema in the namespace up front, so lookup misses surface at startup, not in the request path. Restrict to a subset with `client.WithSchemas("foo", "bar")`.
- **Polling refresh** (default 30s; `client.WithRefreshInterval`). A background goroutine re-fetches only schemas whose current version advanced and atomically swaps the snapshot. Failures are logged and survived (stale-while-error). Force a refresh with `r.Refresh(ctx)`.
- **`r.Pin(ctx, map[string]uint64)`** returns a derived resolver frozen at a specific (`schemaID` → `version`) map — useful for reproducible reads or replaying captured payloads against the exact version they were produced with.
- **`r.Schema(schemaID)`** narrows lookups to one schema in the namespace — cheaper and immune to cross-schema FQN collisions.
- **Fail-loud collisions.** If two schemas in the namespace export the same fully-qualified type name, `client.New` returns an error rather than silently picking one.

Pairs cleanly with [`protowire-go`](https://github.com/trendvidia/protowire-go) (the `pxf` / `sbe` codecs accept any `protoreflect.MessageDescriptor`), `protojson`, `anypb`, and `dynamicpb` without adapter code:

```go
import "github.com/trendvidia/protowire-go/encoding/pxf"

desc, _ := r.FindDescriptorByName("billing.Config")
msg, _  := pxf.UnmarshalDescriptor(pxfBytes, desc.(protoreflect.MessageDescriptor))
```

## Development

```bash
go build ./...                  # Build all packages
go build ./cmd/protoregistry/   # Build the CLI/server binary
go test -race ./...             # All tests (needs Docker for Postgres integration tests)

sqlc generate                   # Regenerate SQL query code
```

### Prerequisites

- Go 1.26+
- Docker (for integration tests)
- `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc` (for proto regeneration)
- `sqlc` (for SQL code regeneration)

## How protoregistry compares

protoregistry is designed for teams that want a schema registry they can
embed, scope per-tenant, and run as a small Go binary against an existing
PostgreSQL — without adopting a broader platform.

| Need                                          | protoregistry | [Buf Schema Registry](https://buf.build/product/bsr) | [Confluent Schema Registry](https://docs.confluent.io/platform/current/schema-registry/index.html) |
|-----------------------------------------------|:-------------:|:----------------------------------------------------:|:--------------------------------------------------------------------------------------------------:|
| Self-hosted (single Go binary + Postgres)     | ✓             | hosted / BSR Pro                                     | ✓ (Kafka-coupled)                                                                                  |
| Multi-tenant namespace isolation (chroot)     | ✓             | modules                                              | subjects                                                                                           |
| Two-phase staging + atomic multi-schema promote | ✓           | drafts                                               | —                                                                                                  |
| Backward-compat enforcement at promote        | ✓             | ✓                                                    | ✓ (per-subject)                                                                                    |
| Embeddable as a Go library                    | ✓             | —                                                    | —                                                                                                  |
| Lock-free hot-swap of compiled descriptors    | ✓             | n/a                                                  | n/a                                                                                                |
| Built-in dynamic message creation             | `dynamicpb`   | —                                                    | —                                                                                                  |
| Wire-format support                           | protobuf      | protobuf                                             | Avro / JSON / protobuf                                                                             |

If you need a polished SaaS, lint rules, code generation, or a wide
ecosystem of integrations, the Buf Schema Registry is the better choice. If
you are already standardized on Kafka, Confluent's registry integrates
natively with the broker. protoregistry's niche is *embed-and-control*: a
small library + service you can run inside your own infrastructure with
strong tenant isolation and a coordinated promotion workflow.

## License

This project is licensed under the [MIT License](LICENSE) — Copyright (c)
2026 TrendVidia, LLC.
