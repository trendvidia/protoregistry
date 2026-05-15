// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package protoregistry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/trendvidia/protoregistry/authz"
	"github.com/trendvidia/protoregistry/compat"
	"github.com/trendvidia/protoregistry/compiler"
	"github.com/trendvidia/protoregistry/namespace"
	"github.com/trendvidia/protoregistry/snapshot"
	"github.com/trendvidia/protoregistry/store"
)

// BuiltinsNamespace is the reserved namespace for built-in proto files.
// Schemas published here are available to all namespaces during compilation.
const BuiltinsNamespace = "__builtins__"

// Registry is the top-level orchestrator that ties together compilation,
// storage, namespace management, and compatibility checking.
type Registry struct {
	store         store.Store
	compiler      *compiler.Compiler
	namespaces    *namespace.Registry
	authz         authz.Authorizer
	authzExplicit bool // set when WithAuthorizer overrode the default
}

// Option configures a Registry.
type Option func(*Registry)

// WithCompiler sets a custom compiler for the registry.
func WithCompiler(c *compiler.Compiler) Option {
	return func(r *Registry) { r.compiler = c }
}

// WithAuthorizer wires a policy-decision layer for privileged operations
// (Publish, Promote, CreateNamespace, SetNamespaceParent, Rebase). The
// default is authz.AllowAll, which permits everything and is suitable
// only for tests and single-tenant local deployments — when in effect,
// the registry logs a warning at startup.
func WithAuthorizer(a authz.Authorizer) Option {
	return func(r *Registry) {
		r.authz = a
		r.authzExplicit = true
	}
}

// New creates a new Registry.
func New(s store.Store, opts ...Option) *Registry {
	r := &Registry{
		store:      s,
		compiler:   compiler.New(),
		namespaces: namespace.NewRegistry(),
		authz:      authz.AllowAll{},
	}
	for _, opt := range opts {
		opt(r)
	}
	if !r.authzExplicit {
		slog.Warn("registry running with authz.AllowAll — every operation is permitted; inject an Authorizer via registry.WithAuthorizer for production")
	}
	return r
}

// Restore loads all current schema versions from the store and rebuilds
// the in-memory namespace state. Call this at startup.
//
// Two passes are required so that namespace parent pointers are
// established before any per-schema work runs. The slow path of
// buildSnapshot (compiler-version mismatch) calls gatherChainTiers,
// which relies on the publishing namespace having its in-memory Chain()
// populated.
func (r *Registry) Restore(ctx context.Context) error {
	// Pass 1: load every namespace and wire its parent pointer. We build
	// the in-memory Namespace objects up front so Get() never returns nil
	// during the second pass when resolving parents from IDs.
	nsRows, err := r.store.ListNamespaces(ctx)
	if err != nil {
		return fmt.Errorf("listing namespaces: %w", err)
	}
	for _, n := range nsRows {
		r.namespaces.GetOrCreate(n.ID)
	}
	for _, n := range nsRows {
		if n.ParentNamespaceID == nil {
			continue
		}
		child := r.namespaces.Get(n.ID)
		parent := r.namespaces.Get(*n.ParentNamespaceID)
		if parent == nil {
			// FK constraint should prevent this in normal operation; if it
			// happens, refusing to start is safer than silently dropping
			// the parent relationship.
			return fmt.Errorf("namespace %s references unknown parent %s",
				n.ID, *n.ParentNamespaceID)
		}
		child.SetParent(parent)
	}

	// Pass 2: load all current schemas and build snapshots.
	schemas, err := r.store.LoadAllCurrent(ctx)
	if err != nil {
		return fmt.Errorf("loading current schemas: %w", err)
	}

	compilerVer := compiler.Version()
	for _, cs := range schemas {
		snap, err := r.buildSnapshot(ctx, cs, compilerVer)
		if err != nil {
			return fmt.Errorf("restoring %s/%s@%d: %w", cs.NamespaceID, cs.SchemaID, cs.Version, err)
		}
		ns := r.namespaces.GetOrCreate(cs.NamespaceID)
		ns.SetCurrent(cs.SchemaID, snap)
	}
	return nil
}

// PublishRequest contains the parameters for publishing a new schema version.
type PublishRequest struct {
	NamespaceID string
	SchemaID    string
	Sources     map[string][]byte // filename → proto source content
	CreatedBy   string
	Metadata    map[string]string
	Force       bool // when true, allows publishing files that shadow well-known types
}

// PublishResult contains the outcome of a publish operation.
type PublishResult struct {
	Version     uint64
	Fingerprint string
	Snapshot    *snapshot.Snapshot
	NoChange    bool // true if the content is identical to the current staged/current version
}

// Publish compiles new proto sources and stages the result. The new version
// is not yet visible to consumers — call Promote to make it current.
//
// The compilation resolves imports against the namespace's proposed state
// (staged versions where they exist, current otherwise), enabling
// coordinated multi-schema changes.
func (r *Registry) Publish(ctx context.Context, req *PublishRequest) (*PublishResult, error) {
	if err := r.authz.CanPublish(ctx, req.NamespaceID, req.SchemaID); err != nil {
		return nil, err
	}
	return r.publishInternal(ctx, req, publishOpts{})
}

// publishOpts configures internal publish behavior that differs between
// the user-facing Publish and the registry-internal Rebase flows.
type publishOpts struct {
	// forceNewVersion bypasses the source-fingerprint shortcut that
	// would otherwise return NoChange when the schema's source files
	// are byte-identical to the current version. Rebase always sets
	// this — its purpose is to refresh per-import pins against the
	// parent's current state, which by definition leaves the source
	// fingerprint unchanged. Without the bypass, every rebase would
	// no-op.
	forceNewVersion bool
}

// publishInternal is the gate-free core of the publish flow. Used by both
// Publish (with authz.CanPublish) and Rebase (with authz.CanRebase) so the
// two operations share the same compile + store path without double-gating.
func (r *Registry) publishInternal(ctx context.Context, req *PublishRequest, opts publishOpts) (*PublishResult, error) {
	if len(req.Sources) == 0 {
		return nil, errors.New("no source files provided")
	}

	// Check for accidental well-known type shadowing.
	if !req.Force {
		for filename := range req.Sources {
			if compiler.IsWellKnownType(filename) {
				return nil, fmt.Errorf(
					"file %q shadows a well-known type; use force=true to override",
					filename,
				)
			}
		}
	}

	// Ensure namespace and schema exist.
	if err := r.ensureNamespaceAndSchema(ctx, req.NamespaceID, req.SchemaID); err != nil {
		return nil, err
	}

	// Ensure the in-memory namespace exists before the chain walk. Safe to
	// call early; GetOrCreate is idempotent and the parent pointer (if any)
	// was established by Restore at startup or by SetNamespaceParent later.
	ns := r.namespaces.GetOrCreate(req.NamespaceID)

	// Gather dependency sources from other schemas in the namespace.
	deps, err := r.gatherDeps(ctx, req.NamespaceID, req.SchemaID)
	if err != nil {
		return nil, fmt.Errorf("gathering dependencies: %w", err)
	}

	// Gather parent-chain tiers (decision D3 / phase 2b). Empty when this
	// namespace is a root.
	chainTiers, err := r.gatherChainTiers(ctx, ns)
	if err != nil {
		return nil, fmt.Errorf("gathering parent chain: %w", err)
	}

	// Gather built-in sources (skip when publishing to builtins itself).
	var builtins []compiler.DepSource
	if req.NamespaceID != BuiltinsNamespace {
		builtins, err = r.gatherBuiltins(ctx)
		if err != nil {
			return nil, fmt.Errorf("gathering builtins: %w", err)
		}
	}

	// Determine next version number.
	versions, err := r.store.ListVersions(ctx, req.NamespaceID, req.SchemaID)
	if err != nil {
		return nil, fmt.Errorf("listing versions: %w", err)
	}
	nextVersion := uint64(1)
	if len(versions) > 0 {
		nextVersion = versions[len(versions)-1] + 1
	}

	// Compile against the resolved chain.
	result, err := r.compiler.Compile(ctx, nextVersion, req.Sources, deps, chainTiers, builtins)
	if err != nil {
		return nil, fmt.Errorf("compilation failed: %w", err)
	}

	// D2: reject if any ancestor defines an FQN that the child also defines.
	// Runs after a successful compile so we have the child's snapshot to
	// compare. Skipped implicitly for root namespaces (Chain() length 1).
	if err := r.checkAncestorFQNConflicts(result.Snapshot, ns); err != nil {
		return nil, err
	}

	// Check for no-change (fingerprint matches current version). The
	// fingerprint is over the schema's source files only, so it stays
	// constant across rebases — Rebase explicitly bypasses this shortcut
	// via opts.forceNewVersion (per-import pins changed even though
	// sources didn't).
	fingerprint := compiler.ComputeFingerprint(result.Files)
	if !opts.forceNewVersion {
		if noChange, err := r.isUnchanged(ctx, req.NamespaceID, req.SchemaID, fingerprint); err != nil {
			return nil, err
		} else if noChange {
			return &PublishResult{NoChange: true, Fingerprint: fingerprint}, nil
		}
	}

	// Store blobs (content-addressable, deduped).
	for _, f := range result.Files {
		err := r.store.PutBlob(ctx, &store.ProtoBlob{
			NamespaceID:    req.NamespaceID,
			SHA256:         f.BlobSHA256,
			OriginalSource: f.OriginalSource,
			SizeBytes:      len(f.OriginalSource),
		})
		if err != nil {
			return nil, fmt.Errorf("storing blob for %s: %w", f.Filename, err)
		}
	}

	// Store version + files + deps.
	versionFiles := make([]store.VersionFile, len(result.Files))
	for i, f := range result.Files {
		versionFiles[i] = store.VersionFile{
			NamespaceID: req.NamespaceID,
			SchemaID:    req.SchemaID,
			Version:     nextVersion,
			Filename:    f.Filename,
			BlobSHA256:  f.BlobSHA256,
		}
	}

	versionDeps := make([]store.VersionDep, len(result.Deps))
	for i, d := range result.Deps {
		versionDeps[i] = store.VersionDep{
			NamespaceID:    req.NamespaceID,
			SchemaID:       req.SchemaID,
			Version:        nextVersion,
			DepNamespaceID: d.DepNamespaceID,
			DepSchemaID:    d.DepSchemaID,
			DepFilename:    d.DepFilename,
			DepVersion:     d.DepVersion,
		}
	}

	err = r.store.PutVersion(ctx, &store.SchemaVersion{
		NamespaceID:     req.NamespaceID,
		SchemaID:        req.SchemaID,
		Version:         nextVersion,
		Compiled:        result.Compiled,
		CompilerVersion: compiler.Version(),
		CreatedBy:       req.CreatedBy,
		Metadata:        req.Metadata,
	}, versionFiles, versionDeps)
	if err != nil {
		return nil, fmt.Errorf("storing version: %w", err)
	}

	// Stage the version.
	if err := r.store.SetStaged(ctx, req.NamespaceID, req.SchemaID, nextVersion); err != nil {
		return nil, fmt.Errorf("staging version: %w", err)
	}

	// Update in-memory staged snapshot. Reuses the ns reference taken
	// before the chain walk.
	ns.SetStaged(req.SchemaID, result.Snapshot)

	return &PublishResult{
		Version:     nextVersion,
		Fingerprint: fingerprint,
		Snapshot:    result.Snapshot,
	}, nil
}

// PromoteResult contains the outcome of a promote operation.
type PromoteResult struct {
	Promoted []store.PromotedSchema
}

// Promote atomically moves all staged versions to current within a namespace.
// Before promoting, it runs backward compatibility checks on each staged
// schema against its current version.
func (r *Registry) Promote(ctx context.Context, namespaceID string) (*PromoteResult, error) {
	if err := r.authz.CanPromote(ctx, namespaceID); err != nil {
		return nil, err
	}
	ns := r.namespaces.Get(namespaceID)
	if ns == nil {
		return nil, fmt.Errorf("namespace %s not found", namespaceID)
	}

	// Run compat checks: staged vs current for each schema with staged changes.
	// Skip the check when the staged version is a rollback (version <= current),
	// since rollbacks are intentional reversions.
	for _, schemaID := range ns.SchemaIDs() {
		staged := ns.Staged(schemaID)
		if staged == nil {
			continue
		}
		current := ns.Current(schemaID)
		if current != nil && staged.Version() <= current.Version() {
			continue // Rollback — skip compat check.
		}
		result := compat.Check(current, staged)
		if !result.OK() {
			return nil, fmt.Errorf("compatibility check failed for %s/%s: %s",
				namespaceID, schemaID, result.Error())
		}
	}

	// Promote in store (atomic).
	promoted, err := r.store.Promote(ctx, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("promoting: %w", err)
	}

	// Swap in-memory snapshots.
	ns.Promote()

	return &PromoteResult{Promoted: promoted}, nil
}

// DiscardStaging clears all staged versions in a namespace without promoting.
func (r *Registry) DiscardStaging(ctx context.Context, namespaceID string) error {
	if err := r.store.DiscardStaging(ctx, namespaceID); err != nil {
		return fmt.Errorf("discarding staging: %w", err)
	}
	if ns := r.namespaces.Get(namespaceID); ns != nil {
		ns.DiscardStaging()
	}
	return nil
}

// RollbackOptions configures a Rollback call.
type RollbackOptions struct {
	// Force bypasses the API-compat check that would otherwise reject a
	// rollback whose target is not backward-compatible with the current
	// version. Use sparingly: skipping the check shifts the cost to
	// downstream consumers, who may break unexpectedly when the rollback
	// is promoted.
	Force bool
}

// Rollback stages a previous version of a schema for promotion.
//
// Unless opts.Force is set, Rollback runs the same compat check as a
// forward Publish, treating the current version as "old" and the target
// version as "new". This catches the common screw-up of rolling back to
// a release that lacks API surface (fields, methods, enum values) added
// in the meantime, which would silently break consumers when the rollback
// is promoted.
//
// The version must already exist in the store.
func (r *Registry) Rollback(ctx context.Context, namespaceID, schemaID string, version uint64, opts RollbackOptions) error {
	ver, err := r.store.GetVersion(ctx, namespaceID, schemaID, version)
	if err != nil {
		return fmt.Errorf("getting version %d: %w", version, err)
	}

	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(ver.Compiled, fds); err != nil {
		return fmt.Errorf("unmarshaling compiled descriptors: %w", err)
	}

	snap, err := snapshot.New(version, fds)
	if err != nil {
		return fmt.Errorf("building snapshot: %w", err)
	}

	// API-compat check vs current. Compat semantics: the second argument
	// must be a backward-compatible replacement for the first. For
	// rollback, the first is "what consumers see today" and the second is
	// "what they will see after promote".
	if !opts.Force {
		current := r.Current(namespaceID, schemaID)
		if current != nil {
			result := compat.Check(current, snap)
			if !result.OK() {
				return fmt.Errorf(
					"rollback to version %d would break consumers of %s/%s: %s; pass force=true to override",
					version, namespaceID, schemaID, result.Error())
			}
		}
	}

	if err := r.store.SetStaged(ctx, namespaceID, schemaID, version); err != nil {
		return fmt.Errorf("setting staged to version %d: %w", version, err)
	}

	ns := r.namespaces.GetOrCreate(namespaceID)
	ns.SetStaged(schemaID, snap)

	return nil
}

// Current returns the current snapshot for a schema, or nil if none.
func (r *Registry) Current(namespaceID, schemaID string) *snapshot.Snapshot {
	ns := r.namespaces.Get(namespaceID)
	if ns == nil {
		return nil
	}
	return ns.Current(schemaID)
}

// Staged returns the staged snapshot for a schema, or nil if none.
func (r *Registry) Staged(namespaceID, schemaID string) *snapshot.Snapshot {
	ns := r.namespaces.Get(namespaceID)
	if ns == nil {
		return nil
	}
	return ns.Staged(schemaID)
}

// Namespaces returns the namespace registry for direct access.
func (r *Registry) Namespaces() *namespace.Registry {
	return r.namespaces
}

// GetNamespaceChain returns the resolution chain for a namespace as an
// ordered slice of namespace IDs, child-first (the namespace itself at
// index 0, then its parent, grandparent, etc.). The implicit
// __builtins__ and Google WKT tiers are not included — they are
// resolver-level concerns, not addressable namespaces.
//
// Read-only and not gated by authz; the chain is structural metadata.
// Returns an error only when the namespace doesn't exist.
func (r *Registry) GetNamespaceChain(_ context.Context, namespaceID string) ([]string, error) {
	ns := r.namespaces.Get(namespaceID)
	if ns == nil {
		return nil, fmt.Errorf("namespace %s not found", namespaceID)
	}
	chain := ns.Chain()
	ids := make([]string, len(chain))
	for i, n := range chain {
		ids[i] = n.ID()
	}
	return ids, nil
}

// CreateNamespaceRequest contains the parameters for creating a namespace.
// Compared to the implicit creation that happens during Publish, this lets
// callers establish a namespace explicitly — typically because they want
// to specify a parent up front. ActorID is recorded in the re-parenting
// audit log when ParentID is non-nil; tests and admin scripts that don't
// have a real principal may pass a sentinel like "system".
type CreateNamespaceRequest struct {
	NamespaceID string
	ParentID    *string
	Metadata    map[string]string
	ActorID     string
}

// CreateNamespace creates a namespace explicitly, optionally with a
// parent in the resolution chain. Gated by authz.CanCreateNamespace.
//
// If ParentID is non-nil, the namespace is created first and then
// re-parented; the re-parenting step is transactional with its audit
// row (decision D9). If the parent doesn't exist, the namespace creation
// is rolled back via SoftDelete... — currently we leave the namespace in
// place because the store doesn't expose a hard-delete primitive. Future
// work: combine create + set-parent into one transactional store call.
func (r *Registry) CreateNamespace(ctx context.Context, req *CreateNamespaceRequest) error {
	if err := r.authz.CanCreateNamespace(ctx, req.NamespaceID, req.ParentID); err != nil {
		return err
	}
	if err := r.store.CreateNamespace(ctx, &store.Namespace{
		ID:       req.NamespaceID,
		Metadata: req.Metadata,
	}); err != nil {
		return fmt.Errorf("creating namespace %s: %w", req.NamespaceID, err)
	}
	// Mirror in-memory.
	ns := r.namespaces.GetOrCreate(req.NamespaceID)

	if req.ParentID == nil {
		return nil
	}
	if err := r.store.SetNamespaceParent(ctx, req.NamespaceID, req.ParentID, req.ActorID); err != nil {
		return fmt.Errorf("setting parent on new namespace: %w", err)
	}
	parent := r.namespaces.Get(*req.ParentID)
	if parent == nil {
		// Parent was loaded as a side-effect of store.SetNamespaceParent's
		// existence check; ensure it's in memory.
		parent = r.namespaces.GetOrCreate(*req.ParentID)
	}
	ns.SetParent(parent)
	return nil
}

// SetNamespaceParentRequest contains the parameters for re-parenting an
// existing namespace.
type SetNamespaceParentRequest struct {
	NamespaceID string
	ParentID    *string // nil clears the parent (reset to implicit root)
	ActorID     string
}

// RebaseStatus describes how far the current version of a schema is from
// the parent state that would result from a fresh rebase. With per-import
// pinning (decision D3), the diff is reported per pinned parent file,
// not per "namespace snapshot" — each pinned file may have moved
// independently in the parent.
type RebaseStatus struct {
	// PinStatuses has one entry per cross-namespace dependency the schema
	// has pinned. Same-namespace deps are not included.
	PinStatuses []ParentPinStatus
	// RebaseAvailable is true when at least one cross-namespace pin is
	// behind the parent's current state.
	RebaseAvailable bool
}

// ParentPinStatus describes one pinned cross-namespace dependency and
// the parent's current version of the same file, so callers can decide
// whether a rebase is needed and what's likely to change.
type ParentPinStatus struct {
	ParentNamespaceID string
	DepSchemaID       string
	DepFilename       string
	PinnedVersion     uint64
	// CurrentVersion is the parent's current version of (DepSchemaID,
	// DepFilename). Zero when the schema or file no longer exists in
	// the parent — in that case rebase will fail because the pinned
	// file vanished. (Future work: report that case more explicitly.)
	CurrentVersion uint64
}

// GetRebaseStatus reports for each cross-namespace pin in the schema's
// current version, whether the parent has moved past it. Idempotent and
// read-only; safe to call without authz.
func (r *Registry) GetRebaseStatus(ctx context.Context, namespaceID, schemaID string) (*RebaseStatus, error) {
	ns := r.namespaces.Get(namespaceID)
	if ns == nil {
		return nil, fmt.Errorf("namespace %s not found", namespaceID)
	}
	cur := ns.Current(schemaID)
	if cur == nil {
		return nil, fmt.Errorf("schema %s/%s has no current version", namespaceID, schemaID)
	}
	deps, err := r.store.GetVersionDeps(ctx, namespaceID, schemaID, cur.Version())
	if err != nil {
		return nil, fmt.Errorf("loading deps for %s/%s@%d: %w", namespaceID, schemaID, cur.Version(), err)
	}

	status := &RebaseStatus{}
	for _, d := range deps {
		if d.DepNamespaceID == namespaceID {
			continue // same-namespace dep, not a rebase concern
		}
		var currentVersion uint64
		parentSchema, err := r.store.GetSchema(ctx, d.DepNamespaceID, d.DepSchemaID)
		if err == nil && parentSchema.CurrentVersion != nil {
			currentVersion = *parentSchema.CurrentVersion
		}
		ps := ParentPinStatus{
			ParentNamespaceID: d.DepNamespaceID,
			DepSchemaID:       d.DepSchemaID,
			DepFilename:       d.DepFilename,
			PinnedVersion:     d.DepVersion,
			CurrentVersion:    currentVersion,
		}
		if currentVersion > d.DepVersion {
			status.RebaseAvailable = true
		}
		status.PinStatuses = append(status.PinStatuses, ps)
	}
	return status, nil
}

// RebaseRequest contains the parameters for rebasing a schema against
// its parent chain's current state.
type RebaseRequest struct {
	NamespaceID string
	SchemaID    string
	// ActorID is recorded in the resulting version's metadata as the
	// principal that triggered the rebase.
	ActorID string
}

// Rebase re-resolves a schema's parent-chain dependencies against the
// parent's *current* state and publishes a new version with refreshed
// per-import pins. Sources are unchanged — pulled from the schema's
// current version on disk. The new version goes through the standard
// publish flow (D2 conflict detection, compat checks, staging).
//
// Gated by authz.CanRebase. Internally uses publishInternal so the
// CanPublish gate doesn't fire — semantically Rebase grants the same
// access as Publish to the same namespace.
//
// Serialization with concurrent publish (D8) is currently provided by
// the database's existing primary-key constraints on schema_versions:
// concurrent attempts to allocate the same next-version number conflict
// and one fails with a unique-violation. A future enhancement could add
// an explicit advisory lock per (namespace, schema) for cleaner error
// reporting.
func (r *Registry) Rebase(ctx context.Context, req *RebaseRequest) (*PublishResult, error) {
	if err := r.authz.CanRebase(ctx, req.NamespaceID, ""); err != nil {
		return nil, err
	}

	ns := r.namespaces.Get(req.NamespaceID)
	if ns == nil {
		return nil, fmt.Errorf("namespace %s not found", req.NamespaceID)
	}
	cur := ns.Current(req.SchemaID)
	if cur == nil {
		return nil, fmt.Errorf("schema %s/%s has no current version to rebase", req.NamespaceID, req.SchemaID)
	}

	files, err := r.store.GetVersionFiles(ctx, req.NamespaceID, req.SchemaID, cur.Version())
	if err != nil {
		return nil, fmt.Errorf("loading version files: %w", err)
	}
	sources := make(map[string][]byte, len(files))
	for _, f := range files {
		blob, err := r.store.GetBlob(ctx, req.NamespaceID, f.BlobSHA256)
		if err != nil {
			return nil, fmt.Errorf("loading blob %s: %w", f.BlobSHA256, err)
		}
		sources[f.Filename] = blob.OriginalSource
	}

	return r.publishInternal(ctx, &PublishRequest{
		NamespaceID: req.NamespaceID,
		SchemaID:    req.SchemaID,
		Sources:     sources,
		CreatedBy:   req.ActorID,
		Metadata:    map[string]string{"rebased_from_version": fmt.Sprintf("%d", cur.Version())},
	}, publishOpts{forceNewVersion: true})
}

// SetNamespaceParent re-parents an existing namespace. Gated by
// authz.CanSetNamespaceParent. The store-level change is transactional
// with the re-parenting audit-log row (decision D9). The in-memory
// parent pointer is updated only after a successful commit.
func (r *Registry) SetNamespaceParent(ctx context.Context, req *SetNamespaceParentRequest) error {
	if err := r.authz.CanSetNamespaceParent(ctx, req.NamespaceID, req.ParentID); err != nil {
		return err
	}
	if err := r.store.SetNamespaceParent(ctx, req.NamespaceID, req.ParentID, req.ActorID); err != nil {
		return err
	}

	child := r.namespaces.GetOrCreate(req.NamespaceID)
	if req.ParentID == nil {
		child.SetParent(nil)
		return nil
	}
	parent := r.namespaces.GetOrCreate(*req.ParentID)
	child.SetParent(parent)
	return nil
}

// --- internal helpers ---

func (r *Registry) ensureNamespaceAndSchema(ctx context.Context, namespaceID, schemaID string) error {
	// Try to get the namespace; create if missing.
	if _, err := r.store.GetNamespace(ctx, namespaceID); err != nil {
		if err := r.store.CreateNamespace(ctx, &store.Namespace{ID: namespaceID}); err != nil {
			// Ignore duplicate — may have been created concurrently.
			if _, err2 := r.store.GetNamespace(ctx, namespaceID); err2 != nil {
				return fmt.Errorf("creating namespace: %w", err)
			}
		}
	}

	// Try to get the schema; create if missing.
	if _, err := r.store.GetSchema(ctx, namespaceID, schemaID); err != nil {
		if err := r.store.CreateSchema(ctx, &store.Schema{
			NamespaceID: namespaceID,
			SchemaID:    schemaID,
		}); err != nil {
			if _, err2 := r.store.GetSchema(ctx, namespaceID, schemaID); err2 != nil {
				return fmt.Errorf("creating schema: %w", err)
			}
		}
	}

	return nil
}

func (r *Registry) gatherDeps(ctx context.Context, namespaceID, schemaID string) ([]compiler.DepSource, error) {
	// Load the proposed state (staged where available, else current) for the namespace.
	proposed, err := r.store.LoadNamespaceProposed(ctx, namespaceID)
	if err != nil {
		return nil, err
	}

	var deps []compiler.DepSource
	for _, cs := range proposed {
		if cs.SchemaID == schemaID {
			continue // Skip self.
		}
		// For each file in the dependency schema, fetch the source blob.
		for _, f := range cs.Files {
			blob, err := r.store.GetBlob(ctx, namespaceID, f.BlobSHA256)
			if err != nil {
				return nil, fmt.Errorf("fetching blob for %s/%s: %w", cs.SchemaID, f.Filename, err)
			}
			deps = append(deps, compiler.DepSource{
				Namespace: namespaceID,
				SchemaID:  cs.SchemaID,
				Version:   cs.Version,
				Filename:  f.Filename,
				Source:    blob.OriginalSource,
			})
		}
	}
	return deps, nil
}

// gatherChainTiers builds the parent-namespace resolver tiers for the
// publishing namespace ns. Walks ns.Chain()[1:] (excluding self) and, for
// each ancestor, loads its current schemas and source blobs from the store.
// Returns nil for namespaces with no parent.
//
// The store is the source of truth here rather than in-memory snapshots,
// so this works during Restore's slow path (when ancestor snapshots may
// not yet be in memory) as well as during Publish (when they are).
//
// Cross-namespace resolution uses the parent's *current* state, not its
// staged state — children should not see another namespace's in-flight
// work. The compiler's per-import deps records (CompileResult.Deps) pin
// the specific versions used here, so the resulting child snapshot is
// reproducible even if the parent's current later changes.
func (r *Registry) gatherChainTiers(ctx context.Context, ns *namespace.Namespace) ([]compiler.ChainTier, error) {
	chain := ns.Chain()
	if len(chain) <= 1 {
		return nil, nil
	}
	ancestors := chain[1:]
	tiers := make([]compiler.ChainTier, 0, len(ancestors))
	for _, ancestor := range ancestors {
		ancestorID := ancestor.ID()
		currents, err := r.store.LoadNamespaceCurrent(ctx, ancestorID)
		if err != nil {
			return nil, fmt.Errorf("loading current state of ancestor %s: %w", ancestorID, err)
		}
		var files []compiler.DepSource
		for _, cs := range currents {
			for _, f := range cs.Files {
				blob, err := r.store.GetBlob(ctx, ancestorID, f.BlobSHA256)
				if err != nil {
					return nil, fmt.Errorf("fetching blob %s from ancestor %s: %w", f.Filename, ancestorID, err)
				}
				files = append(files, compiler.DepSource{
					Namespace: ancestorID,
					SchemaID:  cs.SchemaID,
					Version:   cs.Version,
					Filename:  f.Filename,
					Source:    blob.OriginalSource,
				})
			}
		}
		tiers = append(tiers, compiler.ChainTier{
			NamespaceID: ancestorID,
			Files:       files,
		})
	}
	return tiers, nil
}

// checkAncestorFQNConflicts runs decision D2: same fully-qualified name
// across the chain is a publish-time error. Iterates each ancestor's
// current schemas and reports the first ancestor that defines an FQN
// also defined in the child. The error names the ancestor and the
// conflicting symbols for actionable diagnostics.
func (r *Registry) checkAncestorFQNConflicts(child *snapshot.Snapshot, ns *namespace.Namespace) error {
	chain := ns.Chain()
	if len(chain) <= 1 {
		return nil
	}
	for _, ancestor := range chain[1:] {
		var allConflicts []compiler.FQNConflict
		for _, schemaID := range ancestor.SchemaIDs() {
			ancestorSnap := ancestor.Current(schemaID)
			if ancestorSnap == nil {
				continue
			}
			allConflicts = append(allConflicts, compiler.DetectFQNConflicts(child, ancestorSnap)...)
		}
		if len(allConflicts) > 0 {
			return &compiler.FQNConflictError{
				AncestorNamespaceID: ancestor.ID(),
				Conflicts:           allConflicts,
			}
		}
	}
	return nil
}

func (r *Registry) gatherBuiltins(ctx context.Context) ([]compiler.DepSource, error) {
	current, err := r.store.LoadNamespaceCurrent(ctx, BuiltinsNamespace)
	if err != nil {
		return nil, err
	}
	if len(current) == 0 {
		return nil, nil
	}

	var builtins []compiler.DepSource
	for _, cs := range current {
		for _, f := range cs.Files {
			blob, err := r.store.GetBlob(ctx, BuiltinsNamespace, f.BlobSHA256)
			if err != nil {
				return nil, fmt.Errorf("fetching builtin blob for %s/%s: %w", cs.SchemaID, f.Filename, err)
			}
			builtins = append(builtins, compiler.DepSource{
				Namespace: BuiltinsNamespace,
				SchemaID:  cs.SchemaID,
				Version:   cs.Version,
				Filename:  f.Filename,
				Source:    blob.OriginalSource,
			})
		}
	}
	return builtins, nil
}

func (r *Registry) isUnchanged(ctx context.Context, namespaceID, schemaID, fingerprint string) (bool, error) {
	schema, err := r.store.GetSchema(ctx, namespaceID, schemaID)
	if err != nil {
		return false, nil // Schema doesn't exist yet — definitely changed.
	}

	// Check against staged version first, then current.
	checkVersion := schema.StagedVersion
	if checkVersion == nil {
		checkVersion = schema.CurrentVersion
	}
	if checkVersion == nil {
		return false, nil // No version to compare against.
	}

	files, err := r.store.GetVersionFiles(ctx, namespaceID, schemaID, *checkVersion)
	if err != nil {
		return false, nil
	}

	// Reconstruct file results for fingerprinting.
	var fileResults []compiler.FileResult
	for _, f := range files {
		fileResults = append(fileResults, compiler.FileResult{
			Filename:   f.Filename,
			BlobSHA256: f.BlobSHA256,
		})
	}

	return compiler.ComputeFingerprint(fileResults) == fingerprint, nil
}

func (r *Registry) buildSnapshot(ctx context.Context, cs *store.CurrentSchema, compilerVer string) (*snapshot.Snapshot, error) {
	// If compiler version matches, use cached compiled descriptors.
	if cs.CompilerVersion == compilerVer {
		fds := &descriptorpb.FileDescriptorSet{}
		if err := proto.Unmarshal(cs.Compiled, fds); err != nil {
			return nil, fmt.Errorf("unmarshaling compiled: %w", err)
		}
		return snapshot.New(cs.Version, fds)
	}

	// Compiler version mismatch — recompile from source.
	sources := make(map[string][]byte, len(cs.Files))
	for _, f := range cs.Files {
		blob, err := r.store.GetBlob(ctx, cs.NamespaceID, f.BlobSHA256)
		if err != nil {
			return nil, fmt.Errorf("fetching blob %s: %w", f.BlobSHA256, err)
		}
		sources[f.Filename] = blob.OriginalSource
	}

	// Gather deps from the same namespace.
	deps, err := r.gatherDeps(ctx, cs.NamespaceID, cs.SchemaID)
	if err != nil {
		return nil, fmt.Errorf("gathering deps for recompile: %w", err)
	}

	// Gather parent-chain tiers. Uses the in-memory Chain() established by
	// Restore's first pass, and the store as the source of ancestor files.
	// Known limitation: this uses each ancestor's *current* state, not the
	// pinned versions originally recorded in schema_version_deps, so a
	// compiler-version-change recompile may diverge from the originally
	// stored compiled bytes if a parent has moved forward. Acceptable for
	// 2b — strict reproducibility on compiler bumps is a future-work item.
	ns := r.namespaces.GetOrCreate(cs.NamespaceID)
	chainTiers, err := r.gatherChainTiers(ctx, ns)
	if err != nil {
		return nil, fmt.Errorf("gathering parent chain for recompile: %w", err)
	}

	// Gather builtins (skip when recompiling builtins themselves).
	var builtins []compiler.DepSource
	if cs.NamespaceID != BuiltinsNamespace {
		builtins, err = r.gatherBuiltins(ctx)
		if err != nil {
			return nil, fmt.Errorf("gathering builtins for recompile: %w", err)
		}
	}

	result, err := r.compiler.Compile(ctx, cs.Version, sources, deps, chainTiers, builtins)
	if err != nil {
		return nil, fmt.Errorf("recompiling: %w", err)
	}

	return result.Snapshot, nil
}
