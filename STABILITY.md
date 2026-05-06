# Stability

This document defines the stability surface of `protoregistry`. It tells you
which interfaces will not change incompatibly without a major-version bump,
which interfaces are subject to evolution, and what kinds of changes count as
breaking.

protoregistry is currently a **pre-stable `v0.x` project**. The promises
below describe the contract that will take effect at `v1.0.0`. Until then,
breaking changes can land at any minor version — though we will document them
in [`CHANGELOG.md`](CHANGELOG.md) and avoid them where reasonable.

## Public surfaces

The project has three public surfaces, each with its own contract:

1. **The gRPC service** (`proto/protoregistry/v1/registry.proto`).
2. **The Go library** (`registry.go`, `snapshot/`, `namespace/`, `resolve/`).
3. **The `protoregistry` CLI** (`cmd/protoregistry/`).

### gRPC service — strongest contract

At `v1.0.0`:

- The package path `protoregistry.v1` will be frozen. Incompatible changes
  bump the package to `protoregistry.v2`; the two may coexist indefinitely.
- Field numbers in `RegistryService` request/response messages will not
  change. New fields may be added with new numbers; existing fields may not
  be renumbered, retyped, or removed.
- RPC method names will not change. New methods may be added; existing
  methods may not be removed without a major bump.
- The set of `codes.*` an RPC may return will not narrow. We may begin
  returning a new code only as a result of a previously-unhandled
  precondition surfacing — never as a re-classification of an existing
  failure mode.

### Go library — looser

At `v1.0.0` we will adopt SemVer for the Go module path. Until then:

- Exported types and functions in `registry.go`, `snapshot/`, `namespace/`,
  and `resolve/` may be renamed, restructured, or removed at any minor
  version. Breaking changes will be called out in `CHANGELOG.md`.
- Internal packages (`internal/`) carry no stability guarantee at any
  version, including post-`v1.0.0`.

The intent is for the gRPC service to be the durable integration point, with
the Go library as a convenience for in-process embedding. Heavy library
users should pin to a specific minor version.

### CLI — evolves

The `protoregistry` CLI follows looser rules than the gRPC service:

- New subcommands and flags can be added at any minor version.
- Existing flags are deprecated for one minor version before removal at the
  next major.
- Exit codes are stable: `0` success, `1` user error, `2` internal error.
- The JSON output of `--output json` is treated like the gRPC contract: new
  fields may be added; existing fields may not be renamed or retyped.

## Storage

The PostgreSQL schema in [`migrations/`](migrations/) is an internal
implementation detail. Operators interact with it only via the goose
migration runner. We will provide forward migrations across all version
upgrades; rollbacks across major bumps are not guaranteed.

The compiled `FileDescriptorSet` cached in `schema_versions.descriptor` is
recomputed automatically on startup if `compiler_version` does not match
the running binary, so binary upgrades do not require manual intervention.

## What this does *not* commit to

- **Performance characteristics.** We may make any change that preserves
  the contracts above, even if it regresses runtime or memory.
- **Wire compatibility with other schema registries.** protoregistry's gRPC
  service is its own contract; it intentionally does not implement the
  Confluent or Buf Schema Registry APIs.
- **Database schema as a public surface.** Tools that read the underlying
  `proto_blobs` / `schema_versions` tables directly are working against
  an internal interface and may break without notice.

## Deprecation policy

When something stable must be removed at a future major:

1. **Announce in `CHANGELOG.md`** at the minor where deprecation begins,
   with a clear migration path.
2. **Mark in code** with `// Deprecated:` comments or, for proto fields,
   the `[deprecated = true]` option.
3. **Remove at the next major.** Minimum gap from announcement to removal
   is one minor version, two is preferred.

## Reporting a stability break

If you observe a change that violates the contract above, file an issue
against [`trendvidia/protoregistry`](https://github.com/trendvidia/protoregistry)
with the version pair (before/after) and the smallest reproducer you can
share. Stability regressions are treated as bugs.
