// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)


// cacheManifestVersion is the schema version of the on-disk manifest
// JSON. Bump when the layout changes; readers reject mismatched
// versions and treat the cache as missing rather than crashing.
const cacheManifestVersion = 1

// WithDiskCache configures an on-disk cache directory. Two effects:
//
//   - On every successful populate / Refresh, the Resolver writes
//     each schema's FileDescriptorSet bytes plus a small manifest
//     (namespace, schemas + versions, chain, save timestamp) under
//     <path>/<namespace>/ . Writes are atomic (write-temp + rename).
//   - When [Dial] cannot reach the server, it falls back to loading
//     the most recently persisted snapshot from this directory and
//     returns a Resolver in "stale" mode — see [Resolver.IsStale].
//
// Stale resolvers serve lookups against the cached snapshot but do
// NOT run a refresh loop (nothing to dial against) and return an
// error from [Resolver.Refresh]. Recovering from stale mode
// requires the caller to construct a fresh Resolver — usually on
// the next editor restart, by which point the network may be back.
//
// The cache is per-process: two Resolvers pointed at the same path
// will race on writes. Atomic rename keeps individual reads
// consistent, but you may lose intermediate updates. Document this
// behavior to callers; don't try to file-lock at this layer.
func WithDiskCache(path string) Option {
	return func(c *config) { c.cacheDir = path }
}

// cacheManifest is the on-disk index for one namespace's cached
// descriptors. Stored as JSON for human inspectability; the
// descriptor bytes themselves live in sibling .pb files.
type cacheManifest struct {
	Version     int                   `json:"version"`
	Namespace   string                `json:"namespace"`
	SavedAt     time.Time             `json:"saved_at"`
	Schemas     []cacheSchemaManifest `json:"schemas"`
	ServerChain []string              `json:"server_chain,omitempty"`
}

type cacheSchemaManifest struct {
	SchemaID string `json:"schema_id"`
	Version  uint64 `json:"version"`
	File     string `json:"file"` // relative to <ns-dir>/schemas/
}

// cacheNamespaceDir returns the directory the given namespace's
// cache lives under inside the configured cache root.
func cacheNamespaceDir(cacheRoot, namespace string) string {
	return filepath.Join(cacheRoot, namespace)
}

// persist serializes the current snapshot to disk under
// r.cfg.cacheDir/<namespace>/. No-op when no cache is configured
// or when the snapshot is empty (we don't truncate a previously-
// good cache with an empty one — that pattern indicates a botched
// startup, not a real "all schemas removed" event).
//
// Persistence failures are returned to the caller. The refresh
// loop logs them and continues; the in-memory snapshot stays
// authoritative even when disk persistence breaks.
func (r *Resolver) persist() error {
	if r == nil || r.cfg.cacheDir == "" {
		return nil
	}
	snap := r.snapshot.Load()
	if snap == nil || len(snap.schemas) == 0 {
		return nil
	}

	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()

	nsDir := cacheNamespaceDir(r.cfg.cacheDir, r.ns)
	schemasDir := filepath.Join(nsDir, "schemas")
	if err := os.MkdirAll(schemasDir, 0o755); err != nil {
		return fmt.Errorf("creating cache dir %s: %w", schemasDir, err)
	}

	man := cacheManifest{
		Version:   cacheManifestVersion,
		Namespace: r.ns,
		SavedAt:   time.Now().UTC(),
		Schemas:   make([]cacheSchemaManifest, 0, len(snap.schemas)),
	}
	if len(r.ancestors) > 0 {
		// Nearest-first, matching what GetNamespaceChain returns. The
		// loader uses this to know which ancestor subdirectories to
		// look for when WithServerChain is configured at load time.
		chain := make([]string, 0, len(r.ancestors)+1)
		chain = append(chain, r.ns)
		for _, a := range r.ancestors {
			chain = append(chain, a.ns)
		}
		man.ServerChain = chain
	}

	// <ns-dir>/schemas/<id>@<version>.pb — version in the name so a
	// concurrent reader who somehow opens a stale file still sees
	// coherent bytes for that exact version.
	schemaIDs := make([]string, 0, len(snap.schemas))
	for id := range snap.schemas {
		schemaIDs = append(schemaIDs, id)
	}
	sort.Strings(schemaIDs)
	for _, id := range schemaIDs {
		ss := snap.schemas[id]
		if len(ss.rawDescriptorSet) == 0 {
			// Pre-cache-feature snapshot. Skip rather than fail;
			// next refresh will populate.
			continue
		}
		fname := fmt.Sprintf("%s@%d.pb", id, ss.version)
		dst := filepath.Join(schemasDir, fname)
		if err := atomicWrite(dst, ss.rawDescriptorSet); err != nil {
			return fmt.Errorf("write schema cache %s: %w", dst, err)
		}
		man.Schemas = append(man.Schemas, cacheSchemaManifest{
			SchemaID: id,
			Version:  ss.version,
			File:     fname,
		})
	}

	manBytes, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	manPath := filepath.Join(nsDir, "manifest.json")
	if err := atomicWrite(manPath, manBytes); err != nil {
		return fmt.Errorf("write manifest %s: %w", manPath, err)
	}

	// Persist ancestors too. Each runs through its own persist() so
	// chain caching is just N independent single-namespace caches.
	for _, a := range r.ancestors {
		if err := a.persist(); err != nil {
			return fmt.Errorf("persist ancestor %s: %w", a.ns, err)
		}
	}
	return nil
}

// atomicWrite writes data to path atomically: write to a sibling
// temp file, fsync, rename over. The rename is atomic on POSIX
// and on Windows (Go's os.Rename uses MoveFileEx). Readers see
// either the old or the new contents, never a torn write.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// loadFromCache constructs a stale-mode Resolver from a previously
// persisted snapshot. Returns an error when the cache is missing
// or any descriptor fails to recompile.
//
// The returned Resolver has rpc == nil, refresh disabled, and
// IsStale() == true. Lookups against it are served from the cached
// snapshot exactly as if it had been fetched live.
//
// When useServerChain is set on cfg and the manifest records a
// chain, every ancestor listed is loaded from its own
// subdirectory under the same cacheDir, ancestors first so the
// main namespace's nsFiles / nsTypes can be parent-rooted at
// construction (matching the online newSingleNamespace flow).
// A missing ancestor cache is treated as a load failure — we
// don't silently produce a different lookup surface than live.
func loadFromCache(cacheDir, namespace string, cfg config) (*Resolver, error) {
	if cacheDir == "" {
		return nil, errors.New("loadFromCache: empty cacheDir")
	}

	// Peek at the main manifest first to discover whether a chain
	// was persisted. We need that BEFORE constructing anything if
	// useServerChain is true — ancestors must be loaded first so
	// the main namespace's aggregates can be parent-rooted.
	mainMan, err := readManifest(cacheDir, namespace)
	if err != nil {
		return nil, err
	}

	if !cfg.useServerChain || len(mainMan.ServerChain) <= 1 {
		return loadSingleNamespaceFromCache(cacheDir, namespace, cfg, nil)
	}

	chain := mainMan.ServerChain
	ancestorCfg := cfg
	ancestorCfg.schemas = nil
	ancestorCfg.useServerChain = false

	var parent *Resolver
	ancestors := make([]*Resolver, 0, len(chain)-1)
	for i := len(chain) - 1; i >= 1; i-- {
		a, err := loadSingleNamespaceFromCache(cacheDir, chain[i], ancestorCfg, parent)
		if err != nil {
			for _, prev := range ancestors {
				_ = prev.Close()
			}
			return nil, fmt.Errorf("loading cached ancestor %s: %w", chain[i], err)
		}
		ancestors = append(ancestors, a)
		parent = a
	}

	main, err := loadSingleNamespaceFromCache(cacheDir, namespace, cfg, parent)
	if err != nil {
		for _, a := range ancestors {
			_ = a.Close()
		}
		return nil, err
	}
	main.ancestors = ancestors
	return main, nil
}

// readManifest loads and validates one namespace's manifest from
// disk. Returns a clear error when the file is missing, has an
// unexpected version, or references the wrong namespace —
// callers can distinguish "cold cache" from "corrupted cache" by
// inspecting the wrapped errors.
func readManifest(cacheDir, namespace string) (*cacheManifest, error) {
	manPath := filepath.Join(cacheNamespaceDir(cacheDir, namespace), "manifest.json")
	manBytes, err := os.ReadFile(manPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", manPath, err)
	}
	var man cacheManifest
	if err := json.Unmarshal(manBytes, &man); err != nil {
		return nil, fmt.Errorf("unmarshal manifest %s: %w", manPath, err)
	}
	if man.Version != cacheManifestVersion {
		return nil, fmt.Errorf("manifest %s has version %d; want %d",
			manPath, man.Version, cacheManifestVersion)
	}
	if man.Namespace != namespace {
		return nil, fmt.Errorf("manifest %s namespace mismatch: got %q want %q",
			manPath, man.Namespace, namespace)
	}
	return &man, nil
}

// loadSingleNamespaceFromCache loads one namespace's manifest and
// schema files into a stale-mode Resolver. parent (when non-nil)
// becomes the resolver's fallback parent, mirroring
// newSingleNamespace's flow.
func loadSingleNamespaceFromCache(cacheDir, namespace string, cfg config, parent *Resolver) (*Resolver, error) {
	man, err := readManifest(cacheDir, namespace)
	if err != nil {
		return nil, err
	}

	if parent != nil {
		cfg.parentFiles = parent.nsFiles
		cfg.parentTypes = parent.nsTypes
	}

	r := &Resolver{
		conn:      nil,
		rpc:       nil,
		ns:        namespace,
		cfg:       cfg,
		logger:    resolveLogger(cfg.logger),
		nsFiles:   protoregistry.NewNamespacedFiles(cfg.parentFiles),
		nsTypes:   protoregistry.NewNamespacedTypes(cfg.parentTypes),
		fromCache: true,
	}
	r.cfg.refresh = 0 // no refresh loop in stale mode

	nsDir := cacheNamespaceDir(cacheDir, namespace)
	snap := newSnapshot(len(man.Schemas))
	for _, sm := range man.Schemas {
		// Optional WithSchemas filter still applies — if the caller
		// said WithSchemas("billing"), don't bother loading "audit"
		// off disk even if it's there.
		if !r.tracksSchema(sm.SchemaID) {
			continue
		}
		path := filepath.Join(nsDir, "schemas", sm.File)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read cached schema %s: %w", path, err)
		}
		fdset := &descriptorpb.FileDescriptorSet{}
		if err := proto.Unmarshal(raw, fdset); err != nil {
			return nil, fmt.Errorf("unmarshal cached schema %s: %w", path, err)
		}
		ss, err := r.compileSchema(sm.SchemaID, sm.Version, fdset, raw)
		if err != nil {
			return nil, fmt.Errorf("compile cached schema %s: %w", path, err)
		}
		snap.schemas[sm.SchemaID] = ss
	}
	if err := snap.buildNameIndex(); err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(snap.schemas))
	for id := range snap.schemas {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := r.applyToAggregate(snap.schemas[id]); err != nil {
			return nil, err
		}
	}
	r.snapshot.Store(snap)
	return r, nil
}

// IsStale reports whether the Resolver is serving from a disk cache
// rather than a live registry connection. Stale resolvers' lookups
// work normally but won't reflect any server-side changes since the
// last successful refresh — callers surfacing freshness in their UI
// (e.g. an editor status bar) should consult this.
func (r *Resolver) IsStale() bool {
	if r == nil {
		return false
	}
	return r.fromCache
}

// ErrStaleResolver is returned by Refresh on a Resolver that was
// constructed from a disk cache. Recovering from stale mode
// requires constructing a fresh Resolver via Dial — there's no
// live gRPC connection to refresh against.
var ErrStaleResolver = errors.New("protoregistry/client: resolver loaded from disk cache; refresh not possible")

// dialOnline opens a gRPC connection and constructs an online
// Resolver. Pulled out of [Dial] so the cache-fallback wrapper can
// distinguish "online attempt failed" from "everything failed."
func dialOnline(ctx context.Context, addr, namespace string, cfg config, opts []Option) (*Resolver, error) {
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	if cfg.token != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(bearerCreds{token: cfg.token}))
	}
	conn, err := grpc.NewClient(addr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("protoregistry/client: dialing %s: %w", addr, err)
	}
	r, err := New(ctx, conn, namespace, opts...)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	r.ownsConn = true
	return r, nil
}
