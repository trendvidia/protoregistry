# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once it reaches `v1.0.0`. Until then, breaking changes can land at any minor
version — see [`STABILITY.md`](STABILITY.md).

## Unreleased

### Changed

- **`client` no longer needs the protobuf fork** (#44). A binary importing
  `client` can drop its `replace` of `google.golang.org/protobuf`. A binary
  that replaces it with trendvidia/protobuf-go still builds.

  **Breaking:** the namespaced registries moved to the new `nsregistry`
  package, so `WithFallback` now takes `*nsregistry.NamespacedFiles` and
  `*nsregistry.NamespacedTypes`. Change `protoregistry.NewNamespacedFiles` and
  its neighbours to `nsregistry`.

  Errors from those registries now start with `nsregistry:` instead of
  `proto:`. The legacy MessageSet lookup, which only ran under the
  `protolegacy` build tag, is gone.

- **The two bundled protowire schemas now come from `trendvidia/protos`** (#45),
  so this module no longer requires protowire. The copies add
  `go_package` and the Java and C# options, so generated code from a stored
  schema resolves to the `protos` packages. Only versions compiled from now on
  carry them. Source hashes are unchanged.

## [0.73.0] - 2026-10-29

Schemas written with RFC-001 syntax (Protowire Schema Extensions) now
publish, with no setup. The registry compiles with the trendvidia/protocompile
fork, stores the RFC-001 carriers unchanged, and bundles the two protowire
schemas such schemas import. The gRPC API is published to the Buf Schema
Registry, and the server ships as a container image on GHCR. Source hashes
are unchanged, and the client API has no changes.

### Added

- **Two protowire schemas are bundled** (#33):
  `protowire/schema/v1/annotations.proto`, which every schema using RFC-001
  syntax imports, and `pxf/annotations.proto`, for the PXF field options
  `(pxf.required)`, `(pxf.default)` and `(pxf.key)`. A schema importing
  either now publishes with no setup. It used to fail with "imported file
  does not exist" until an operator published the file to `__builtins__`.
  They resolve after `__builtins__` (an operator's own copy still wins) and
  before Google's well-known types. The copies come from the
  `github.com/trendvidia/protowire` module (v1.13.0), which raises grpc to
  v1.84.0 and golang.org/x/net to v0.58.0 through its requirements.
- **The gRPC API is on the Buf Schema Registry** as
  [`buf.build/trendvidia/protoregistry`](https://buf.build/trendvidia/protoregistry)
  (#37), pushed on each release tag and labelled with it. A client in any
  language can generate its stubs from there; the README's gRPC API section
  shows the `deps` entry and the managed-mode setting Go clients need.
- **Container image on GHCR**: `ghcr.io/trendvidia/protoregistry`, built for
  `linux/amd64` and `linux/arm64` (#18, #19, #20). A release tag publishes
  `X.Y.Z` (no `v`), `X.Y` and `latest`; `main` publishes a rolling `main`
  tag. This release: `ghcr.io/trendvidia/protoregistry:0.73.0`.

### Changed

- **Schemas are compiled with the trendvidia/protocompile fork**
  (v0.35.1, from `bufbuild/protocompile` v0.14.1), so a schema written
  with RFC-001 syntax (Protowire Schema Extensions: annotations,
  `function` and `type` declarations) publishes. The compiler lowers that
  syntax into the carrier options (`protowire.schema.v1`, fields
  1327–1331) and protoregistry stores them unchanged: the stored
  descriptors equal what the toolchain alone produces, byte for byte, and
  they come back byte-identical through `Restore` and `GetDescriptor`.
  The schema's `import "protowire/schema/v1/annotations.proto"` resolves
  from the bundled copy (see Added).
  - Source hashes (deduplication and change detection) are unchanged: of
    1,227 distinct `.proto` files, every one that both parsers accept
    hashes the same. Fourteen proto2 files with unlabelled fields are now
    rejected when parsed rather than when compiled; they were rejected
    before too.
- **protowire-go v1.0.0 → v1.12.0** (used by `protoregistry ctl` for PXF
  output).

### Chores

- Dependency upgrades: grpc to v1.84.0 (#26, #30, and through protowire),
  pgx v5.10.0 (#27), testcontainers v0.43.0 (#29); GitHub Actions
  `actions/checkout` v7 and the Docker build actions (#21–#25, #28).

## [0.72.0] - 2026-05-19

Small additive release: TLS transport credentials for `Dial`.
Enables protolsp and other clients to dial registries across
networks while preserving `Dial`'s cache-fallback behavior on the
failure path. No client breakage.

### Added — client

- **`client.WithTransportCredentials(creds)`** (#16): supply gRPC
  transport credentials for `Dial`, replacing the hard-coded
  `insecure.NewCredentials()` that has been the only behavior since
  the package shipped. Default unchanged (no option set ⇒ insecure
  transport, fine for loopback / docker-compose). Production callers
  pass `credentials.NewTLS(tlsCfg)` to enable TLS while keeping the
  cache-fallback path on `Dial` failure. Callers using `New` are
  unaffected — they own the `*grpc.ClientConn` and configure
  credentials there directly, as before.

### Chores

- Gitignore the `protoregistry` binary at the repo root (#15).

## [0.71.0] - 2026-05-16

Substantial feature release covering server-side namespace
hierarchy plus a wave of client SDK additions (server-chain
resolution, namespace provenance, source fetch, offline disk
cache). All changes are additive; no client breakage.

### Added — server

- **Namespace hierarchy** (#8): namespaces can declare a parent
  via `SetNamespaceParent`, forming a chain that schema-resolution
  walks at publish, compile, and read time. Includes:
  - New RPCs `SetNamespaceParent` / `GetNamespaceChain`.
  - `internal/authz` package for capability-based parent-chain
    access checks. `WithAuthorizer` injects a production
    authorizer; the default `authz.AllowAll` is for development
    only and logs a warning on startup.
  - Rebase machinery (`Rebase` / `GetRebaseStatus`) for replaying
    descendants when an ancestor changes, with audit-log entries
    per affected schema.
  - FQN-collision detection across the chain at publish time.

### Added — client

- **`client.WithServerChain()`** (#9): construct a Resolver that
  consults `GetNamespaceChain` and automatically wires ancestor
  Resolvers, so a single `client.New(..., namespace)` resolves
  types declared anywhere in the namespace's chain.
- **`Resolver.RangeMessages(f func(MessageType) bool)`** (#10):
  iterate every message type currently visible to the resolver —
  bound namespace plus chain ancestors. Used by editor tooling
  building completion lists for the `@type` directive.
- **`Resolver.FindMessageByNameWithOrigin` /
  `Resolver.FindFileByPathWithOrigin`** (#11): like their
  non-Origin counterparts but also return the namespace ID that
  contributed the type / file. Useful for editor integrations
  rendering provenance ("defined in namespace acme-shared") next
  to hover or completion results. Empty origin string for files
  reachable only via ad-hoc parents (`WithParent` /
  `WithFallback` / `WithGlobalFallback`).
- **`Resolver.GetSource(ctx, filePath)`** (#12): fetch the
  original `.proto` source bytes for a file the resolver knows.
  Locates the owning schema across the chain, calls the gRPC
  `GetSource` RPC, returns the requested file's bytes. Editor
  integrations use this to render registry-only files as virtual
  documents in go-to-definition. Returns `NotFound` for files
  reachable only through ad-hoc parents (no registry connection
  on those tiers).
- **`client.WithDiskCache(path)`** (#13): opt-in on-disk
  descriptor cache. Successful populate / Refresh writes each
  schema's `FileDescriptorSet` bytes plus a manifest under
  `<path>/<namespace>/`. When `Dial` cannot reach the server,
  loads the most recently persisted snapshot and returns a
  stale-mode Resolver. `Resolver.IsStale()` reports the mode;
  `Resolver.Refresh()` on a stale Resolver returns the new
  `ErrStaleResolver` sentinel. Atomic writes (temp + rename);
  ancestors persist into sibling subdirectories under the same
  cacheDir; the top-level manifest records the chain so the
  offline loader can reconstruct it without server access.

### Changed

- `client.Dial` flow restructured into a try-online-then-fallback
  shape when `WithDiskCache` is configured. Without that option
  the behavior is unchanged — dial failures surface as-is.
- `client/snapshot.go`: `fetchSchema` now defers to a new
  `compileSchema(schemaID, version, fdset, raw)` helper so the
  offline cache loader can compile from disk bytes through the
  same path. `schemaSnapshot` gains `rawDescriptorSet []byte`
  (the wire bytes the snapshot was built from) so the disk
  persister can write them out without re-marshaling.

### Dependencies

- `google.golang.org/grpc` minor + patch bumps via dependabot
  (#2, #6).
- `github.com/jackc/pgx/v5` 5.9.1 → 5.9.2 (#5).
- `github.com/pressly/goose/v3` 3.27.0 → 3.27.1 (#6).
- `testcontainers` group bump (#3).
- `github.com/trendvidia/protowire-go` bumped to v1.0.0 (#7) —
  pxf v1 grammar.

## [0.70.1] - 2026-05-06

### Added

- **`client.WithFallback(files, types)`** — configure parent
  `*protoregistry.NamespacedFiles` / `*NamespacedTypes` registries
  that the Resolver falls back to on a local miss. The
  namespace-wide aggregate and every per-schema view inherit the
  same parent, so well-known or shared types are visible from every
  lookup tier (`FindDescriptorByName`, `FindMessageByName`,
  `FindFileByPath`, `FindExtensionByName`, `FindExtensionByNumber`,
  and `SchemaResolver.FindMessageByName`).
- **`client.WithParent(parent *Resolver)`** — convenience that
  chains another Resolver as the parent (passes its `nsFiles` /
  `nsTypes` through as fallback). Useful for modeling a "common
  types" namespace as the parent of per-tenant namespaces.
- **`client.WithGlobalFallback()`** — fall back to upstream
  `protoregistry.GlobalFiles` / `GlobalTypes` so the Resolver can
  resolve both registry-managed and statically-compiled proto types
  through the same API.

### Changed

- The Resolver-level `Find*` methods no longer short-circuit to
  `NotFound` on a local-name-index miss. They now delegate to the
  Resolver's `nsFiles` / `nsTypes` aggregates, which transparently
  walk the configured parent chain via the fork's
  hierarchical-fallback machinery. Behavior is unchanged when no
  fallback is configured (no parent → still `NotFound`).

### Internal

- `client/snapshot.go`: per-schema `*NamespacedFiles` /
  `*NamespacedTypes` are constructed with `cfg.parentFiles` /
  `cfg.parentTypes` as parents, so per-schema lookups reach the
  fallback chain too.
- `client/client.go`: `Pin` inherits the parent's fallback
  configuration so well-known / shared types stay visible in
  pinned views. Documented the caveat that a pinned view over a
  still-refreshing parent will see new parent entries surface after
  Pin returns; callers wanting a fully-frozen pin must build an
  independent frozen parent and pass it via `WithFallback`.

### Tests

- `TestIntegration/WithFallback_ResolvesParentTypes` exercises the
  full fallback chain across all lookup tiers, including
  `SchemaResolver`, with a "commons" namespace configured as parent
  of a "fallback" namespace via `client.WithParent`.

## [0.70.0] - 2026-05-06

First public release. Aligns with the `v0.70.0` cut of the trendvidia
protowire stack.

### Added

#### Open-source infrastructure
- `LICENSE` (MIT) and per-file SPDX headers on all hand-written Go files.
- `SECURITY.md` with the vulnerability disclosure process.
- `CONTRIBUTING.md` covering the dev loop and code conventions.
- `STABILITY.md` declaring the v0 pre-stable contract.
- `CODE_OF_CONDUCT.md` (Contributor Covenant v2.1).
- `Makefile` with `vet`, `lint`, `test`, `test-unit`, `headers`,
  `headers-check`, `generate`, `build` targets.
- `scripts/add-license-header.sh` for adding/checking SPDX headers.
- GitHub Actions CI: `vet`, `lint` (golangci-lint), `build`, unit tests,
  integration tests (testcontainers Postgres), header check.
- Dependabot config for weekly Go module updates.
- `.editorconfig` and `.golangci.yml` for editor/linter consistency.
- `.github/ISSUE_TEMPLATE/` (bug, feature, security/discussion redirect)
  and `PULL_REQUEST_TEMPLATE.md`.
- `.github/CODEOWNERS` assigning every path to the maintainer.
- `examples/client-grpc/` and `examples/client-sdk/` runnable end-to-end
  examples covering the producer (raw gRPC) and consumer (Go SDK) sides.
- `CONTRIBUTING.md` documents the Steward-governed PR review and merge
  process (lifecycle, escrow, mentorship mode, slash commands).

#### Security and operational hardening
- gRPC auth seam: pluggable `server.Authenticator` interface with built-in
  `NoAuth` (default) and `TokenAuth` (bearer tokens from a file)
  implementations, wired via `server.UnaryAuthInterceptor`.
- TLS / mTLS support on the `serve` command: `--tls-cert`, `--tls-key`,
  `--tls-client-ca`.
- Symmetric TLS / mTLS / bearer-token support on the CLI client: `--tls`,
  `--tls-ca`, `--tls-cert`, `--tls-key`, `--tls-server-name`,
  `--tls-skip-verify`, `--token` (env: `PROTOREGISTRY_TOKEN`). Without
  these, an operator who locked the server down with `--tls-cert` could
  not use the CLI against it.
- `server.Identity` model: subject + admin flag attached to every request
  context, consumable via `server.IdentityFromContext`.
- Privilege gates: writes to the reserved `__builtins__` namespace and any
  use of `Publish.force=true` or `Rollback.force=true` now require
  `Identity.Admin = true`.
- Anonymous-write gating via `--insecure-allow-anonymous-writes`
  (default `true` for back-compat; the server emits a startup WARN when
  running unauthenticated).
- Input validation at the RPC boundary: ID charset/length, filename
  traversal/NUL/leading-slash rejection, per-file source size cap, total
  source size cap, file count cap.
- gRPC server limits: `--max-recv-msg-size` / `--max-send-msg-size`,
  keepalive enforcement.
- Compiler safety: `compiler.WithTimeout` (default 30s),
  `compiler.WithMaxFileSourceBytes` (default 8 MiB),
  `compiler.WithMaxFiles` (default 1000), enforced before any AST work.
- Audit logging on every write RPC via `slog` (subject, namespace, schema,
  version, force flag, file count).
- Rollback compat check: `Registry.Rollback` now runs `compat.Check` against
  the current snapshot and rejects API-breaking rollbacks unless
  `RollbackOptions.Force = true`. The server gates `force=true` behind
  admin and emits a WARN audit line when used.
- DB URL passwords are masked (`u:***@host`) in startup log lines.
- CLI confirmation prompts on `discard` and `rollback` commands; bypass
  with `--yes`. Non-interactive shells must pass `--yes` explicitly.

#### Pagination
- `ListSchemas` and `ListNamespaces` RPCs gained `page_size` /
  `page_token` request fields and `next_page_token` response fields.
  Server applies `DefaultListPageSize=100`, `MaxListPageSize=1000`.
  Cursor encoding is opaque base64 and uses keyset (not offset) ordering
  for stability under concurrent writes.
- New store interface methods `ListNamespacesPage` and `ListSchemasPage`
  (PostgreSQL impl + sqlc queries).

#### Tests
- Unit tests for `server/validate.go`, `server/auth.go`, `server/limits.go`.
- Compiler limit and timeout tests (`compiler/limits_test.go`).
- End-to-end gRPC tests via bufconn + testcontainers Postgres
  (`server/server_test.go`): publish→promote→rollback round-trip, validation
  rejections, anonymous-write gating, `__builtins__` admin gate, force admin
  gate, pagination cursor coverage, error sanitization.
- Hot-swap monotonicity test under `-race`
  (`namespace/namespace_test.go`).

### Changed
- README title lowercased to match the rest of the protowire stack
  (`# protoregistry`).
- README expanded with Stability, Comparison-to-alternatives, and
  resolver-chain detail; `--force` flag rationale documented.
- Server gRPC errors now sanitized: backend errors are logged at ERROR with
  full context, but RPC responses return generic `codes.Internal "<op> failed"`
  with no leak of PostgreSQL or wrapped error detail. Typed errors
  (`NotFound`, `InvalidArgument`, `FailedPrecondition`, `PermissionDenied`,
  `Unauthenticated`) are preserved.
- `server.New` constructor now accepts variadic `Option` arguments
  (`WithAuth`, `WithLimits`, `WithLogger`, `WithAllowAnonymousWrites`).
  **Breaking** for any external caller; in-tree callers updated.
- `Registry.Rollback` signature changed from
  `Rollback(ctx, ns, schema, version)` to
  `Rollback(ctx, ns, schema, version, RollbackOptions)`.
  **Breaking** for any external caller; in-tree callers and tests updated.
- `internal/ctl/load.go`: `failed` map now stores wrapped `error` values
  rather than stringified messages, preserving `errors.Is`/`errors.As`
  semantics.

### Fixed
- `Promote` failures caused by compat checks are now classified as
  `codes.FailedPrecondition` rather than `codes.Internal`, so callers can
  distinguish "your request is invalid" from "the server hit an unexpected
  error".
- `protowire-go` dependency pinned to the published `v0.70.0` tag; the
  monorepo `replace` directive has been removed. `go get` now works for
  external users without a sibling checkout.
