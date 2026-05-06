# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once it reaches `v1.0.0`. Until then, breaking changes can land at any minor
version — see [`STABILITY.md`](STABILITY.md).

## Unreleased

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
