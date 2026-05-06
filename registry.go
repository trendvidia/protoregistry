// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package protoregistry

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

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
	store      store.Store
	compiler   *compiler.Compiler
	namespaces *namespace.Registry
}

// Option configures a Registry.
type Option func(*Registry)

// WithCompiler sets a custom compiler for the registry.
func WithCompiler(c *compiler.Compiler) Option {
	return func(r *Registry) { r.compiler = c }
}

// New creates a new Registry.
func New(s store.Store, opts ...Option) *Registry {
	r := &Registry{
		store:      s,
		compiler:   compiler.New(),
		namespaces: namespace.NewRegistry(),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Restore loads all current schema versions from the store and rebuilds
// the in-memory namespace state. Call this at startup.
func (r *Registry) Restore(ctx context.Context) error {
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

	// Gather dependency sources from other schemas in the namespace.
	deps, err := r.gatherDeps(ctx, req.NamespaceID, req.SchemaID)
	if err != nil {
		return nil, fmt.Errorf("gathering dependencies: %w", err)
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

	// Compile.
	result, err := r.compiler.Compile(ctx, nextVersion, req.Sources, deps, builtins)
	if err != nil {
		return nil, fmt.Errorf("compilation failed: %w", err)
	}

	// Check for no-change (fingerprint matches current version).
	fingerprint := compiler.ComputeFingerprint(result.Files)
	if noChange, err := r.isUnchanged(ctx, req.NamespaceID, req.SchemaID, fingerprint); err != nil {
		return nil, err
	} else if noChange {
		return &PublishResult{NoChange: true, Fingerprint: fingerprint}, nil
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
			NamespaceID: req.NamespaceID,
			SchemaID:    req.SchemaID,
			Version:     nextVersion,
			DepSchemaID: d.DepSchemaID,
			DepFilename: d.DepFilename,
			DepVersion:  d.DepVersion,
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

	// Update in-memory staged snapshot.
	ns := r.namespaces.GetOrCreate(req.NamespaceID)
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
				SchemaID: cs.SchemaID,
				Version:  cs.Version,
				Filename: f.Filename,
				Source:   blob.OriginalSource,
			})
		}
	}
	return deps, nil
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
				SchemaID: cs.SchemaID,
				Version:  cs.Version,
				Filename: f.Filename,
				Source:   blob.OriginalSource,
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

	// Gather builtins (skip when recompiling builtins themselves).
	var builtins []compiler.DepSource
	if cs.NamespaceID != BuiltinsNamespace {
		builtins, err = r.gatherBuiltins(ctx)
		if err != nil {
			return nil, fmt.Errorf("gathering builtins for recompile: %w", err)
		}
	}

	result, err := r.compiler.Compile(ctx, cs.Version, sources, deps, builtins)
	if err != nil {
		return nil, fmt.Errorf("recompiling: %w", err)
	}

	return result.Snapshot, nil
}
