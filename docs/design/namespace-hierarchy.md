# Namespace Hierarchy and Chained Resolution

| Field    | Value                                       |
|----------|---------------------------------------------|
| Status   | Draft                                       |
| Date     | 2026-05-14                                  |
| Scope    | `namespace/`, `resolve/`, `compiler/`, `registry.go`, `proto/`, `migrations/`, `server/`, `store/` |
| Related  | `protolsp` (downstream consumer, blocked on this work) |

## Summary

Today every `Namespace` in protoregistry is an isolated container: schemas
within a namespace can import each other, cross-namespace imports are
forbidden, and type resolution stays inside a single namespace. The client
SDK supports ad-hoc fallback chaining via `WithParent` / `WithFallback`
(v0.70.1), but the server has no notion of namespace hierarchy — there is no
authoritative source of "developer X's namespace inherits from org Y's
shared namespace."

This document proposes adding a **single-parent namespace hierarchy** at the
server level, with chained type resolution at compile time and at runtime.
The motivating use case is organization-shared types ("org-level WKT"):
a developer's namespace can resolve fully-qualified type names against a
parent namespace owned by their organization, transparently, without any
explicit import in the schema files themselves.

## Background

### Current state

Verified by audit of the codebase as of 2026-05-14:

- `namespace/namespace.go`: `Namespace` is a flat container keyed by ID;
  no parent reference. Doc comment explicitly states "cross-namespace
  imports are not allowed."
- `resolve/resolve.go`: `Resolver` walks only `r.ns.AllCurrent()` — schemas
  inside one namespace.
- `compiler/compiler.go:243-277`: `buildResolverChain` is a fixed three-tier
  chain (namespace sources → `__builtins__` → Google WKT).
- `registry.go:120-132`: `Publish` gathers deps only from "the" namespace
  plus `__builtins__`.
- `migrations/001_initial.sql`: `namespaces` table has no parent column.
- `proto/protoregistry/v1/registry.proto`: `NamespaceInfo` and
  `CreateNamespaceRequest` have no parent fields.
- `client/client.go:454-516`: client-side `WithParent`, `WithFallback`,
  `WithGlobalFallback` are implemented and tested (`client/integration_test.go:204-261`).
  These are **runtime-only**; the server is unaware of the chain.

### The problem with client-side-only chaining

If the client configures a chain that disagrees with what the server
"thinks" the chain should be, four failure modes arise:

1. **Shorter client chain** — false positives (LSP red on valid pfx).
2. **Longer client chain** — false negatives (LSP green on pfx that fails
   server-side).
3. **Different parent** — silent type drift (same FQN, different bytes).
4. **Different order** — silent override flip.

Cases 2–4 are silent in the worst case and can corrupt wire-format data.
A schema registry that allows ambiguity in name resolution is unsound.

The decision recorded here is to make the server the **authoritative source
of the resolution chain**, eliminating the divergence class entirely.

## Goals

- Authoritative, server-enforced namespace hierarchy.
- Deterministic, reproducible type resolution at compile time and at
  runtime.
- No silent ambiguity: same FQN cannot resolve differently depending on
  who is asking or when.
- Reproducible builds: a child snapshot resolves identically forever,
  regardless of subsequent parent changes.
- Backwards-compatible migration: existing flat namespaces continue to work
  without modification.

## Non-goals

- Multiple inheritance (DAG hierarchies). Out of scope; linear chain only.
- Cross-namespace imports in `.proto` files. Imports remain
  namespace-local; the chain is a resolution mechanism, not an import
  mechanism.
- Per-developer ad-hoc chain overrides. The chain is namespace policy, not
  developer preference.
- Organization data model. Who is a member of which org is the concern of
  an auth/identity layer or a separate org service, not this design.

## Design decisions

The following decisions are load-bearing. Rationale is recorded so future
contributors do not have to re-derive them.

### D1. Linear single-parent hierarchy

Each namespace has at most one parent. The resolution chain is a singly
linked list: `child → parent → grandparent → ... → __builtins__ → Google WKT`.

**Why:** Deterministic resolution order with no diamond-inheritance
disambiguation. A DAG would require explicit ordering rules at every fork,
which is a footgun. The "team within an org" use case can always be
modeled as two levels of linear hierarchy (team's parent = org).

**How to apply:** `parent_namespace_id` is a single nullable column;
`Namespace.Parent()` returns `*Namespace`, never a slice.

### D2. Same fully-qualified name across the chain is a publish-time error

If a child namespace defines a type with the same FQN as any ancestor in
its chain, `Publish` rejects it with a clear diagnostic naming the
shadowed type and the conflicting ancestor.

**Why:** Silent shadowing is the most insidious source of bugs in
inheritance hierarchies. Publish-time rejection makes the conflict
impossible to ignore. Cheap to relax later if a real need for explicit
override emerges; expensive to retract permissive defaults once data has
been written against them.

**How to apply:** Compiler walks the parent chain during dependency
gathering and aborts on FQN collision. Error includes both definition
sites.

### D3. Parent versions pinned per-import at child publish; explicit rebase to upgrade

When a child namespace publishes a new schema version, every file it
resolves through the parent chain is recorded in `schema_version_deps`
with a `dep_namespace_id` identifying where the file came from. The child
compiles against those pinned per-import versions forever.

To pick up changes from the parent, the child must explicitly `Rebase` —
re-compile against the parent's current state, which produces a new child
version with new pin records.

**Why:** Reproducible builds. Without pinning, an org-namespace promotion
silently invalidates every child compiled against the old version — the
exact silent-drift class this design is intended to eliminate. With
pinning, child snapshots are immutable in their resolution, and parent
upgrades become deliberate operations visible in audit history.

**How to apply:** Extend the existing `schema_version_deps` table with a
nullable-during-migration-then-NOT-NULL `dep_namespace_id` column. Same-
namespace dependencies (today's only case) get `dep_namespace_id =
namespace_id` after backfill. Cross-namespace dependencies set
`dep_namespace_id` to the ancestor that contributed the file. The pin
is implicit in the dep rows — no separate "namespace snapshot" concept,
no new table. `Rebase` is a new RPC that creates a new child version
with updated pin records. CI policies may require children to be
rebased against current parents before merge, but the registry does not
enforce currency by default.

Alternative shapes considered (and rejected):

- *Per-namespace `promotion_seq`*: requires recording which schema
  versions were current at each promotion, which ends up duplicating
  `schema_version_deps` with extra coordination cost.
- *Side table of resolved-closure manifests*: explicit but introduces a
  parallel data model for state that the existing deps table already
  captures.

The chosen shape (extending `schema_version_deps`) reuses an existing
concept rather than introducing a new one.

### D4. `__builtins__` becomes the implicit root

Today `__builtins__` is a special-cased third tier in `buildResolverChain`.
With hierarchy, every namespace whose `parent_namespace_id` is `NULL`
implicitly has `__builtins__` as its parent, and `__builtins__`'s implicit
parent is the Google WKT resolver.

**Why:** Removes special-casing from the compiler. The resolver becomes
"walk the chain to the end," uniform for all namespaces. Existing
namespaces with `NULL` parent retain identical behavior to today.

**How to apply:** Compiler's resolver-chain builder walks parents
generically; the `__builtins__` special case is deleted. The implicit
behavior is enforced in the chain-walker, not in the data model — i.e.
`__builtins__` does not appear as a literal row referencing itself.

### D5. Privileged operations gated by a pluggable authorizer

Re-parenting and other policy-sensitive operations are gated by an
`Authorizer` interface that integrating applications implement against
their own identity / OAuth / RBAC infrastructure. The registry itself
defines the *seam*, not the policy. See the "Permission interface"
section below for the full interface definition, actor model, and wiring.

**Why:** The trust model only works if developers cannot escape org
policy by re-pointing their fallback. But the registry should not encode
any specific identity system — different deployments will have different
auth stacks (OAuth, OIDC, mTLS, SSO, RBAC), and the registry must remain
embeddable in all of them.

**How to apply:** Every privileged operation (`SetNamespaceParent`,
`CreateNamespace` with a parent, `Publish`, `Promote`, `Rebase`) calls
into `Authorizer` before mutating state. A default `AllowAll`
implementation ships in the registry for tests and single-tenant
deployments. Production deployments inject their own via
`registry.WithAuthorizer(...)`.

### D6. Rebase is explicit and inspectable

The parent-upgrade workflow is:

1. `GetRebaseStatus(namespace_id)` reports the child's currently pinned
   parent snapshot vs. the parent's current snapshot, and summarizes the
   delta (added/removed/changed types).
2. `Rebase(namespace_id, target_parent_snapshot_id)` re-pins and triggers
   a recompile of the child against the new parent snapshot. Fails with
   a diagnostic if the rebase would introduce a D2 conflict or break any
   child schema.

**Why:** Parent upgrades are the highest-risk operation in this design —
they can break children. Making the operation explicit, dry-runnable, and
reported in audit history is how we keep that risk visible.

**How to apply:** Two new RPCs (`GetRebaseStatus`, `Rebase`). Rebase
publishes a new child snapshot (it does not mutate existing snapshots).

### D7. Filename-based chain resolution; no namespace-qualified imports

Source files use the same `import "foo/bar.proto";` syntax as today. The
import statement names a *filename*, not a namespace. The resolver walks
the namespace chain by filename, child-first, and returns the first
file whose path matches — same model as Go's GOPATH or Python's
`sys.path`. Imports never name another namespace.

**Why:** Preserves the original intent that schemas don't have to know
where their dependencies live — they just `import` what they need and
the registry's hierarchy answers "from where." A schema can be moved
between namespaces (within the same chain) without source changes.
Avoids inventing a namespace-qualified import syntax that would diverge
from upstream protobuf.

**How to apply:** `compiler.buildResolverChain` produces an N-tier
resolver where each tier is the contributed source set of one namespace
in the chain. Tiers are tried in order: child sources → parent sources →
... → `__builtins__` → Google WKT. The first tier whose filename
matches wins.

D2's FQN-uniqueness check (same fully-qualified type name across the
chain) is a separate post-compile pass over defined symbols, not part
of import resolution. It catches the case where the child and an
ancestor each define `acme.commons.Money` even when neither one
imports the other.

### D8. Rebase is serialized with publish per namespace

Concurrent `Rebase` and `Publish` on the same namespace are serialized
through the existing per-namespace publish lock. Rebase acquires the same
lock that `Publish` does and runs its recompile under it.

**Why:** Rebase produces a new child snapshot by recompiling the child
against a different parent snapshot. If a publish lands concurrently
without ordering, the resulting state is undefined — one operation could
silently overwrite the other, or the recompile could run against a
moving target. Reusing the existing publish serialization is the
smallest correct primitive and avoids introducing a second lock model.

**How to apply:** Whatever lock currently protects `Publish` (per
namespace) is acquired by `Rebase` as well. No new locking primitive.

### D9. Re-parenting events are recorded in an append-only audit log

Every successful `SetNamespaceParent` call is recorded with actor,
timestamp, previous parent, and new parent in a dedicated audit table,
populated within the same transaction as the parent update.

**Why:** Re-parenting is high-impact and low-frequency — exactly the
class of operation whose history is asked for after the fact ("when did
this namespace start inheriting from `acme-shared`? who approved it?").
Reconstructing it from generic event logs is unreliable. The append-only
shape ensures history cannot be silently rewritten.

**How to apply:** A new table (suggested name `namespace_parent_events`)
with columns `(namespace_id, previous_parent_id, new_parent_id,
actor_id, occurred_at)`. INSERT-only; never updated or deleted. Same
transaction as the parent-update write ensures the audit row cannot be
lost on rollback.

## Data model

### Database

New column on `namespaces` (shipped in phase 1, `migrations/002_namespace_hierarchy.sql`):

```sql
ALTER TABLE namespaces
  ADD COLUMN parent_namespace_id TEXT REFERENCES namespaces(id);
```

Cycle prevention: a recursive CTE in the `SetNamespaceParent` query
(`store/postgres/queries/namespaces.sql`) atomically checks that the
proposed parent is not a descendant of the namespace being re-parented.
A simple `CHECK` constraint cannot express transitive cycles; the CTE
runs in the same statement as the UPDATE so the check is atomic with
the write. Depth-bounded at 64 levels as a defense against any
pre-existing cycle that might survive a faulty rehydration.

#### Parent-import pinning (shipped in phase 2a)

D3 is implemented by extending the existing `schema_version_deps` table
with a `dep_namespace_id` column. Same-namespace dependencies (the only
case before this work) carry `dep_namespace_id = namespace_id` after
the phase-2a migration. Cross-namespace dependencies set
`dep_namespace_id` to the ancestor that contributed the imported file.

```sql
ALTER TABLE schema_version_deps
    ADD COLUMN dep_namespace_id TEXT REFERENCES namespaces(id);

UPDATE schema_version_deps SET dep_namespace_id = namespace_id;

ALTER TABLE schema_version_deps
    ALTER COLUMN dep_namespace_id SET NOT NULL;

-- Primary key gains the new column so two ancestors that contribute the
-- same (dep_schema_id, dep_filename) for a child don't collide.
ALTER TABLE schema_version_deps DROP CONSTRAINT schema_version_deps_pkey;
ALTER TABLE schema_version_deps ADD PRIMARY KEY
    (namespace_id, schema_id, version, dep_namespace_id, dep_schema_id, dep_filename);
```

The pin is implicit in the dep rows: "this child version imported
`common/money.proto` from namespace `acme-shared` at version 3."
Reproducible because rebuilding the child loads the exact pinned
versions, not whatever is current in the parent today.

### Proto API

```proto
message NamespaceInfo {
  string id = 1;
  google.protobuf.Timestamp created_at = 2;
  map<string, string> metadata = 3;
  optional string parent_namespace_id = 4;  // new
}

message CreateNamespaceRequest {
  string id = 1;
  map<string, string> metadata = 2;
  optional string parent_namespace_id = 3;  // new
}

message SetNamespaceParentRequest {
  string namespace_id = 1;
  optional string parent_namespace_id = 2;  // NULL/unset clears the parent
}

message GetNamespaceChainRequest {
  string namespace_id = 1;
}

message GetNamespaceChainResponse {
  // Ordered child → root. Does not include the implicit __builtins__
  // or Google WKT tiers.
  repeated NamespaceInfo chain = 1;
}

message GetRebaseStatusRequest {
  string namespace_id = 1;
}

message GetRebaseStatusResponse {
  string pinned_parent_snapshot_id = 1;
  string current_parent_snapshot_id = 2;
  bool rebase_available = 3;
  // Type-level delta between pinned and current parent.
  repeated TypeChange parent_changes = 4;
}

message RebaseRequest {
  string namespace_id = 1;
  string target_parent_snapshot_id = 2;
}
```

New RPCs on the registry service:

- `SetNamespaceParent(SetNamespaceParentRequest) → NamespaceInfo`
- `GetNamespaceChain(GetNamespaceChainRequest) → GetNamespaceChainResponse`
- `GetRebaseStatus(GetRebaseStatusRequest) → GetRebaseStatusResponse`
- `Rebase(RebaseRequest) → Snapshot`

### Core types

`namespace.Namespace`:

```go
type Namespace struct {
    id      string
    parent  atomic.Pointer[Namespace]  // new; nil means root (→ __builtins__)
    schemas sync.Map
}

func (ns *Namespace) Parent() *Namespace { return ns.parent.Load() }
func (ns *Namespace) Chain() []*Namespace { /* walk parents */ }
```

Atomic pointer because re-parenting must be safe under concurrent reads.

`resolve.Resolver` extends its lookup methods to walk the chain on miss,
mirroring the client SDK's existing `WithParent` semantics. The
descriptor returned by chain-walking lookups carries an origin annotation
(which namespace it was found in), so downstream tools (LSP hover,
diagnostics) can surface provenance.

## Resolution semantics

For any lookup of a fully-qualified name `N` in namespace `C` with
chain `C → P → G → __builtins__ → google WKT`:

1. Search `C`'s current snapshots for `N`.
2. If miss, search `P`'s current snapshots for `N` — but in the
   **child-publish-time pinned snapshot of `P`**, not `P`'s live current.
3. Recurse through `G`, `__builtins__`, Google WKT.
4. Return the first hit, annotated with origin namespace.
5. If no hit, return `protoregistry.NotFound`.

At publish time, the compiler performs the chain walk once and bakes the
resolved closure into the child snapshot, so step (2)'s pinned lookup is
constant-time at runtime.

## Permission interface

The registry does not implement an identity model. It exposes an
`Authorizer` interface that integrating applications implement against
their own OAuth / OIDC / mTLS / SSO / RBAC infrastructure. The registry
defines the seam; the deployment defines the policy.

### Package location

New top-level package: `authz/`. Public, importable by integrations.

### Actor identity

The acting principal is carried in `context.Context`. The registry does
not define the actor type and does not extract it from any transport.
Integrations:

1. Define their own actor type (struct, opaque token, whatever fits).
2. Provide a transport interceptor (gRPC `UnaryServerInterceptor`, HTTP
   middleware, etc.) that resolves the principal and stores it in
   context under a key the deployment owns.
3. Implement `Authorizer` to read from that key.

The registry never inspects the actor itself. This keeps identity
extraction, policy decisions, and registry invariants as three separate
concerns.

### Interface

```go
package authz

import (
    "context"
    "errors"
)

// Authorizer decides whether the principal carried in ctx may perform a
// given operation. Returns nil to allow; any non-nil error denies the
// operation and is propagated to the caller. Implementations SHOULD wrap
// or return ErrPermissionDenied so callers can distinguish authorization
// failures from other errors.
type Authorizer interface {
    CanCreateNamespace(ctx context.Context, namespaceID string, parentID *string) error
    CanSetNamespaceParent(ctx context.Context, namespaceID string, newParentID *string) error
    CanPublish(ctx context.Context, namespaceID, schemaID string) error
    CanPromote(ctx context.Context, namespaceID string) error
    CanRebase(ctx context.Context, namespaceID, targetParentSnapshotID string) error
}

var ErrPermissionDenied = errors.New("permission denied")
```

The interface is explicit-per-operation rather than a generic
`Check(action, resource)`:

- New parameters per operation are type-checked at compile time.
- Each signature documents exactly what the policy decision has
  available.
- Integrations can embed a default and override only the methods that
  matter to them.

A nil `*string` for `parentID` / `newParentID` means "no parent" (root
namespace). Implementations should treat the presence of a parent as the
trigger for org-membership checks.

### Default implementation

```go
// AllowAll permits every operation. Default when no Authorizer is
// configured. Suitable for tests and single-tenant local deployments
// only. Production deployments MUST inject a real Authorizer.
type AllowAll struct{}

func (AllowAll) CanCreateNamespace(context.Context, string, *string) error      { return nil }
func (AllowAll) CanSetNamespaceParent(context.Context, string, *string) error   { return nil }
func (AllowAll) CanPublish(context.Context, string, string) error                { return nil }
func (AllowAll) CanPromote(context.Context, string) error                        { return nil }
func (AllowAll) CanRebase(context.Context, string, string) error                 { return nil }
```

If `WithAuthorizer` is not called, `AllowAll` is used and a warning is
logged at startup. Refuse-by-default would be safer in isolation but
would break the existing flat-namespace deployments that have no auth
configured today; the warning is the compromise.

### Operations gated

| Operation             | Method                     | Notes |
|-----------------------|----------------------------|-------|
| Create namespace      | `CanCreateNamespace`       | `parentID` non-nil → request includes a parent; policy may require parent be in actor's org |
| Set/change parent     | `CanSetNamespaceParent`    | Highest-privilege; controls fallback resolution. See D5 |
| Publish to namespace  | `CanPublish`               | Existing operation, retroactively wrapped |
| Promote staging       | `CanPromote`               | Existing operation, retroactively wrapped |
| Rebase                | `CanRebase`                | High-impact; recompiles child against new parent. See D6 |

Read operations (`Get*`, `List*`, `Resolve*`) are **not** gated in this
design. Authorization of reads is deferred; it is additive and can be
added later without breaking the write-path interface defined here.

### Wiring

```go
reg := registry.New(
    registry.WithAuthorizer(myAuth),  // optional; defaults to authz.AllowAll
    // ...
)
```

The registry calls `authz.CanX(ctx, ...)` at the entry point of each
gated operation, before any state mutation. On non-nil return, the
operation is aborted and the error is propagated unchanged.

## Migration plan

1. Schema migration adds `parent_namespace_id` to `namespaces` and
   `parent_snapshot_id` to `snapshots`, both nullable.
2. Existing rows retain `NULL` — meaning "root namespace, parent is
   implicit `__builtins__`." Behavior is bit-identical to today.
3. New `SetNamespaceParent` RPC available immediately after deploy; orgs
   can re-parent their developer namespaces at their own pace.
4. The pre-existing client-side `WithParent` / `WithFallback` API
   continues to work; these are now redundant for org-WKT use but remain
   useful for tests and ad-hoc scenarios. No deprecation in this phase.

## Phasing

| Phase | Scope | Risk |
|-------|-------|------|
| 1 | DB migration (`parent_namespace_id` on `namespaces`, `namespace_parent_events` audit table) + proto field additions (`parent_namespace_id` on `NamespaceInfo` and `CreateNamespaceRequest`) + `Namespace.parent` pointer with `Chain()` accessor + store-interface additions (`SetNamespaceParent`, `RecordNamespaceParentEvent`, `ListNamespaceParentEvents`, unused by callers — wired in phase 3). | Low — additive only; landed 2026-05-15 |
| 2a | Migration 003 (`dep_namespace_id` on `schema_version_deps`, backfilled and made `NOT NULL`, added to PK). `DepSource` gains `Namespace`. `compiler.Compile` accepts a parent chain. `compiler.buildResolverChain` becomes N-tier. D2 FQN-conflict detector lives in `compiler/` as a helper. Tests in isolation; no caller wires the new behavior. | Low — additive; one caller change passing `nil` |
| 2b | `Registry.Publish` walks `Namespace.Chain()`, loads each parent's current snapshots, passes them as chain tiers to the compiler, records cross-namespace deps. D2 enforced at publish time via `checkAncestorFQNConflicts`. `Restore` is two-pass: first loads namespaces and rebuilds parent pointers from `parent_namespace_id`, then loads schemas. `buildSnapshot`'s slow path (compiler-version mismatch) also passes chain tiers. Integration tests cover hierarchical publish, D2 rejection, and parent-pointer restore. | Medium — core invariant changes; landed 2026-05-15 |
| 2c | `resolve.Resolver` walks `Namespace.Chain()` on lookup miss. New `*WithOrigin` variants return the namespace ID that contributed each descriptor (for protolsp hover provenance). Existing methods preserved as wrappers, also benefit from chain walking. Tests cover local hit, single-parent fallback, multi-ancestor fallback, nearest-wins on collision, miss, backward-compat of non-origin API. | Low — additive; landed 2026-05-15 |
| 3 | `authz` package with `Authorizer` interface + `AllowAll` default + `ErrPermissionDenied`. `Registry.WithAuthorizer` option (defaults to `AllowAll` with a startup warning). `CanPublish`, `CanPromote`, `CanCreateNamespace`, `CanSetNamespaceParent` wired at registry method entry. New `Registry.CreateNamespace` (with optional parent) and `Registry.SetNamespaceParent` methods. `store.SetNamespaceParent` made transactional with audit-log write (D9). `SetNamespaceParent` RPC added; `CreateNamespace` RPC handler reads `parent_namespace_id`. Integration tests cover audit-log persistence, cycle prevention, self-reference rejection, and authz gating. | Low — additive; landed 2026-05-15 |
| 4 | `Registry.Rebase` + `Registry.GetRebaseStatus` per-schema methods (the per-import pinning model from D3 makes rebase naturally per-schema, not per-namespace as originally sketched). `RebaseRequest`/`GetRebaseStatusRequest` RPCs added; new `ParentPinStatus` message reports pinned vs current parent version per cross-namespace dep. `publishInternal` split out so Rebase reuses the publish flow without double-gating authz. Two latent bugs fixed in passing: (a) the compiled FDS now includes all transitively-imported files so Restore's fast path works for any schema with imports (parent-chain or same-namespace); (b) the D2 detector now ignores same-file FQN matches (the legitimate "child imports parent file" case) and only flags FQNs defined in different files (true shadowing). | Medium — touches publish path; landed 2026-05-15 |
| 5 | Client SDK alignment; deprecate `WithParent` for org use cases (keep for tests) | Low |
| 6 | `protolsp` consumes `GetNamespaceChain` for hover provenance and resolution | Downstream — unblocked by phase 4 |

## Open questions

None at this revision. Previous open questions were resolved as follows:

- *Permission hook shape* → defined as the `Authorizer` interface in the
  "Permission interface" section. Concrete implementation is the
  deployment's responsibility.
- *Rebase atomicity vs. concurrent publish* → resolved by D8: rebase uses
  the existing per-namespace publish serialization.
- *Audit history for re-parenting* → resolved by D9: dedicated append-only
  audit table.

## Alternatives considered

### A1. Client-side chain only (Path C)

Rejected in favor of this design. The client-side `WithParent` API
already exists, but without server enforcement, client and server can
disagree about the chain, producing silent type drift. Documented in the
"problem with client-side-only chaining" section above.

### A2. Multiple parents (DAG)

Rejected (see D1). Adds disambiguation complexity for negligible
expressiveness gain; "team within an org" is expressible as a two-level
linear chain.

### A3. Live parent resolution (no pinning)

Rejected (see D3). Causes silent breakage of children when parent
namespaces promote new versions, which is the exact failure mode this
design eliminates.

### A4. Permissive override on FQN collision

Rejected (see D2). Permissive defaults cannot be retracted after
production data has been encoded against the loose semantics. Strict
default with an explicit override directive (if ever needed) is the safe
order of operations.
