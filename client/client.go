// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	registrypb "github.com/trendvidia/protoregistry/proto/protoregistry/v1"
)

// DefaultRefreshInterval is the cadence at which a Resolver polls the
// server for current-version changes when no explicit interval is set.
const DefaultRefreshInterval = 30 * time.Second

// Resolver resolves protobuf descriptors for a single namespace from a
// remote protoregistry server.
//
// It implements [protoregistry.MessageTypeResolver],
// [protoregistry.ExtensionTypeResolver], and the descriptor lookup half
// of protodesc.Resolver, so it drops into protojson, anypb, dynamicpb,
// and protowire-go without adapter code.
//
// A Resolver is namespace-scoped to mirror the server model. Construct
// one Resolver per namespace.
type Resolver struct {
	conn     *grpc.ClientConn
	ownsConn bool
	rpc      registrypb.RegistryServiceClient
	ns       string
	cfg      config
	logger   *slog.Logger

	// snapshot is the per-schema view: schemaID → schemaSnapshot, plus
	// a cross-schema FQN → schemaID name index. Atomically swapped on
	// refresh; readers always see a coherent set of per-schema views.
	snapshot atomic.Pointer[nsSnapshot]

	// nsFiles and nsTypes are the namespace-wide aggregates that back
	// FindFileByPath and FindExtensionByNumber. They live on the
	// Resolver (not on the snapshot) so refresh can mutate them
	// incrementally via UpdateFile / UnregisterFile rather than
	// rebuilding from scratch on every poll. Reads are protected by
	// the fork's per-instance RWMutex inside each registry; writes are
	// serialized by refreshMu below.
	//
	// One subtle consequence: during a refresh, lookups via
	// FindFileByPath/FindExtensionByNumber may briefly observe the new
	// aggregate state while [Resolver.snapshot] still points at the
	// pre-refresh per-schema view (or vice versa, depending on
	// ordering). For schema-consistent reads, use [Resolver.Schema]
	// (which goes through the snapshot only) or [Resolver.Pin] (which
	// returns a fully-frozen Resolver).
	nsFiles *protoregistry.NamespacedFiles
	nsTypes *protoregistry.NamespacedTypes

	refreshMu sync.Mutex // serializes Refresh calls and aggregate mutations

	cancel context.CancelFunc
	wg     sync.WaitGroup

	// ancestors holds Resolvers for each ancestor namespace when this
	// Resolver was constructed with WithServerChain. Closed in reverse
	// order on Close so refresh goroutines stop cleanly. Empty when
	// no chain expansion was performed.
	ancestors []*Resolver

	// fromCache marks Resolvers constructed via the disk-cache
	// fallback path inside [Dial]. Stale Resolvers have rpc == nil,
	// no refresh goroutine, and return ErrStaleResolver from
	// Refresh. Lookups against the cached snapshot work normally.
	// See [WithDiskCache] / [Resolver.IsStale].
	fromCache bool

	// cacheMu serializes disk-cache writes so two refreshes can't
	// interleave their atomic-rename sequences. Only used when
	// r.cfg.cacheDir != "" — zero-value mutex is fine when
	// caching is disabled.
	cacheMu sync.Mutex
}

// SchemaResolver narrows lookups to a single schema within a namespace.
// Use it when the caller knows which schema owns a type — it skips the
// cross-schema name index and is unaffected by collisions across schemas.
type SchemaResolver struct {
	parent   *Resolver
	schemaID string
}

// New constructs a Resolver bound to the given namespace on an
// already-dialed gRPC connection. The conn is owned by the caller, who
// is responsible for its lifecycle, transport credentials, interceptors,
// and observability hooks.
//
// On success, the returned Resolver has eagerly populated descriptors
// for every schema in the namespace (or the subset selected via
// [WithSchemas]) and started its background refresh goroutine. Call
// [Resolver.Close] to stop it.
func New(ctx context.Context, conn *grpc.ClientConn, namespace string, opts ...Option) (*Resolver, error) {
	if conn == nil {
		return nil, errors.New("protoregistry/client: nil grpc.ClientConn")
	}
	if namespace == "" {
		return nil, errors.New("protoregistry/client: empty namespace")
	}

	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}

	if !cfg.useServerChain {
		r, err := newSingleNamespace(ctx, conn, namespace, cfg, nil)
		if err != nil {
			return nil, err
		}
		if cfg.cacheDir != "" {
			if perr := r.persist(); perr != nil {
				r.logger.Warn("initial cache persist failed",
					"namespace", namespace, "err", perr)
			}
		}
		return r, nil
	}

	// Chain expansion: ask the registry which namespaces are ancestors,
	// then construct a Resolver per ancestor (sharing the same conn) and
	// wire the nearest ancestor as the parent of the main Resolver.
	rpc := registrypb.NewRegistryServiceClient(conn)
	resp, err := rpc.GetNamespaceChain(ctx, &registrypb.GetNamespaceChainRequest{NamespaceId: namespace})
	if err != nil {
		return nil, fmt.Errorf("protoregistry/client: fetching namespace chain for %s: %w", namespace, err)
	}
	chain := resp.NamespaceIds
	if len(chain) == 0 || chain[0] != namespace {
		return nil, fmt.Errorf("protoregistry/client: malformed chain for %s: %v", namespace, chain)
	}

	// Ancestors inherit the auth config (token, logger) but not the
	// schema filter or the chain flag — they load their full namespace
	// and don't recurse into their own ancestors (the chain already
	// walks the full hierarchy).
	ancestorCfg := cfg
	ancestorCfg.schemas = nil
	ancestorCfg.useServerChain = false

	var parent *Resolver
	ancestors := make([]*Resolver, 0, len(chain)-1)
	for i := len(chain) - 1; i >= 1; i-- {
		a, err := newSingleNamespace(ctx, conn, chain[i], ancestorCfg, parent)
		if err != nil {
			for _, prev := range ancestors {
				_ = prev.Close()
			}
			return nil, fmt.Errorf("protoregistry/client: loading ancestor %s: %w", chain[i], err)
		}
		ancestors = append(ancestors, a)
		parent = a
	}

	main, err := newSingleNamespace(ctx, conn, namespace, cfg, parent)
	if err != nil {
		for _, a := range ancestors {
			_ = a.Close()
		}
		return nil, err
	}
	main.ancestors = ancestors
	if cfg.cacheDir != "" {
		// persist() walks ancestors, so this single call writes
		// every namespace's snapshot plus the top-level manifest
		// (with chain recorded). Failures log but don't fail
		// construction — the in-memory snapshot is authoritative.
		if perr := main.persist(); perr != nil {
			main.logger.Warn("initial cache persist failed",
				"namespace", namespace, "err", perr)
		}
	}
	return main, nil
}

// newSingleNamespace constructs a Resolver for one namespace without any
// chain expansion. If parent is non-nil, its namespace-wide registries
// become this Resolver's parent for fallback lookups (overriding any
// cfg.parentFiles/cfg.parentTypes set via WithFallback/WithParent —
// the server-derived chain takes precedence).
func newSingleNamespace(ctx context.Context, conn *grpc.ClientConn, namespace string, cfg config, parent *Resolver) (*Resolver, error) {
	if parent != nil {
		cfg.parentFiles = parent.nsFiles
		cfg.parentTypes = parent.nsTypes
	}

	r := &Resolver{
		conn:    conn,
		rpc:     registrypb.NewRegistryServiceClient(conn),
		ns:      namespace,
		cfg:     cfg,
		logger:  resolveLogger(cfg.logger),
		nsFiles: protoregistry.NewNamespacedFiles(cfg.parentFiles),
		nsTypes: protoregistry.NewNamespacedTypes(cfg.parentTypes),
	}

	if err := r.populate(ctx); err != nil {
		return nil, err
	}

	if cfg.refresh > 0 {
		bg, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		r.wg.Add(1)
		go r.refreshLoop(bg)
	}

	return r, nil
}

// Dial is a convenience constructor that opens an insecure gRPC connection
// and constructs a Resolver in one call. Production callers should
// usually build a *grpc.ClientConn themselves and pass it to [New].
//
// When [WithDiskCache] is configured, Dial persists the snapshot
// after the initial populate and on every successful refresh; if
// the online attempt fails outright (network unreachable, server
// down), Dial falls back to loading the most recent persisted
// snapshot from the cache and returns a stale-mode Resolver. See
// [Resolver.IsStale]. When no cache is configured, the network
// failure is returned as-is.
func Dial(ctx context.Context, addr, namespace string, opts ...Option) (*Resolver, error) {
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}

	r, onlineErr := dialOnline(ctx, addr, namespace, cfg, opts)
	if onlineErr == nil {
		// newSingleNamespace already persisted the initial snapshot
		// when cacheDir is set, so the online path is complete here.
		return r, nil
	}
	if cfg.cacheDir == "" {
		return nil, onlineErr
	}
	cached, cacheErr := loadFromCache(cfg.cacheDir, namespace, cfg)
	if cacheErr != nil {
		return nil, fmt.Errorf("registry unreachable (%v) and cache load failed (%v)", onlineErr, cacheErr)
	}
	cached.logger.Warn("registry unreachable; serving from disk cache",
		"namespace", namespace,
		"address", addr,
		"err", onlineErr)
	return cached, nil
}

// Close stops the background refresh goroutine. If the Resolver was
// created via [Dial] it also closes the underlying gRPC connection;
// otherwise the conn was passed in by the caller and is left alone.
//
// When the Resolver was constructed with [WithServerChain], Close also
// stops the refresh goroutines of every ancestor Resolver it created.
// Ancestors share the same gRPC connection, so conn closure (if owned)
// happens once at the end.
func (r *Resolver) Close() error {
	if r == nil {
		return nil
	}
	if r.cancel != nil {
		r.cancel()
		r.wg.Wait()
	}
	for _, a := range r.ancestors {
		_ = a.Close()
	}
	if r.ownsConn && r.conn != nil {
		return r.conn.Close()
	}
	return nil
}

// Namespace returns the namespace this Resolver is bound to.
func (r *Resolver) Namespace() string {
	if r == nil {
		return ""
	}
	return r.ns
}

// Schema returns a SchemaResolver scoped to a single schema in the
// namespace. The returned resolver shares the parent's cache and
// refresh loop.
func (r *Resolver) Schema(schemaID string) *SchemaResolver {
	return &SchemaResolver{parent: r, schemaID: schemaID}
}

// Pin returns a derived Resolver frozen at the given (schemaID -> version)
// mapping. The parent Resolver is unaffected and continues to track
// current versions. Pinned Resolvers are intended for reproducible
// reads, e.g. replaying a captured PXF stream against the exact schema
// version it was produced with.
//
// The returned Resolver shares the parent's gRPC connection. Closing
// the pinned Resolver does not affect the parent or the conn.
func (r *Resolver) Pin(ctx context.Context, versions map[string]uint64) (*Resolver, error) {
	if len(versions) == 0 {
		return nil, errors.New("protoregistry/client: empty pin map")
	}

	// Pinned Resolver inherits the parent's fallback configuration so
	// well-known / shared types remain visible. Pin doesn't refresh, so
	// inheriting a live parent that does refresh means the pinned view
	// can still see new entries surface in the parent over time —
	// callers wanting a fully-frozen view should construct an
	// independent frozen parent and pass it via [WithFallback].
	pinned := &Resolver{
		conn:     r.conn,
		ownsConn: false,
		rpc:      r.rpc,
		ns:       r.ns,
		cfg:      r.cfg,
		logger:   r.logger,
		nsFiles:  protoregistry.NewNamespacedFiles(r.cfg.parentFiles),
		nsTypes:  protoregistry.NewNamespacedTypes(r.cfg.parentTypes),
	}
	pinned.cfg.refresh = 0
	pinned.cfg.token = r.cfg.token

	snap := newSnapshot(len(versions))
	for schemaID, version := range versions {
		ss, err := pinned.fetchSchema(ctx, schemaID, version)
		if err != nil {
			return nil, fmt.Errorf("pinning %s/%s@%d: %w", r.ns, schemaID, version, err)
		}
		snap.schemas[schemaID] = ss
	}
	if err := snap.buildNameIndex(); err != nil {
		return nil, err
	}

	// Pinned Resolvers are independent of the parent's aggregate; they
	// build their own from scratch and never refresh, so there is no
	// incremental path here.
	ids := make([]string, 0, len(snap.schemas))
	for id := range snap.schemas {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := pinned.applyToAggregate(snap.schemas[id]); err != nil {
			return nil, err
		}
	}

	pinned.snapshot.Store(snap)
	return pinned, nil
}

// --- protoregistry.MessageTypeResolver ---

// FindMessageByName looks up a message type by its fully-qualified name
// across all schemas in the namespace. Falls back to the parent
// registry chain when configured via [WithFallback] / [WithParent] /
// [WithGlobalFallback].
//
// Returns [protoregistry.NotFound] when neither the local namespace
// nor any configured parent defines the name.
func (r *Resolver) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	snap := r.snapshot.Load()
	if snap != nil {
		if ss, ok := snap.schemaFor(name); ok {
			return ss.types.FindMessageByName(name)
		}
	}
	if r.nsTypes != nil {
		return r.nsTypes.FindMessageByName(name)
	}
	return nil, protoregistry.NotFound
}

// FindMessageByURL looks up a message type by its type URL (e.g.
// "type.googleapis.com/billing.v1.Config"). Enables use with
// [google.golang.org/protobuf/types/known/anypb].
func (r *Resolver) FindMessageByURL(url string) (protoreflect.MessageType, error) {
	name := url
	if i := strings.LastIndexByte(url, '/'); i >= 0 {
		name = url[i+1:]
	}
	return r.FindMessageByName(protoreflect.FullName(name))
}

// --- protoregistry.ExtensionTypeResolver ---

// FindExtensionByName looks up an extension by its fully-qualified
// name. Falls back to the parent registry chain when configured.
func (r *Resolver) FindExtensionByName(name protoreflect.FullName) (protoreflect.ExtensionType, error) {
	snap := r.snapshot.Load()
	if snap != nil {
		if ss, ok := snap.schemaFor(name); ok {
			return ss.types.FindExtensionByName(name)
		}
	}
	if r.nsTypes != nil {
		return r.nsTypes.FindExtensionByName(name)
	}
	return nil, protoregistry.NotFound
}

// FindExtensionByNumber looks up an extension by the message it extends
// and its field number. Goes through the Resolver's namespace-wide
// aggregate, which is mutated incrementally on each refresh.
func (r *Resolver) FindExtensionByNumber(message protoreflect.FullName, field protoreflect.FieldNumber) (protoreflect.ExtensionType, error) {
	if r == nil || r.nsTypes == nil {
		return nil, protoregistry.NotFound
	}
	return r.nsTypes.FindExtensionByNumber(message, field)
}

// --- protodesc.Resolver ---

// FindFileByPath looks up a file descriptor by its proto path (e.g.
// "billing/v1/billing.proto"). Goes through the Resolver's
// namespace-wide aggregate, which is mutated incrementally on each
// refresh.
func (r *Resolver) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	if r == nil || r.nsFiles == nil {
		return nil, protoregistry.NotFound
	}
	return r.nsFiles.FindFileByPath(path)
}

// FindDescriptorByName looks up any descriptor (message, enum, service,
// extension, etc.) by its fully-qualified name. Falls back to the
// parent registry chain when configured.
func (r *Resolver) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	snap := r.snapshot.Load()
	if snap != nil {
		if ss, ok := snap.schemaFor(name); ok {
			return ss.files.FindDescriptorByName(name)
		}
	}
	if r.nsFiles != nil {
		return r.nsFiles.FindDescriptorByName(name)
	}
	return nil, protoregistry.NotFound
}

// --- ergonomics ---

// NewMessage constructs an empty dynamic message for the given
// fully-qualified name. Equivalent to looking up the descriptor and
// passing it to [dynamicpb.NewMessage], but bundled into one call
// because callers almost always want the dynamic message, not the
// descriptor itself.
func (r *Resolver) NewMessage(name protoreflect.FullName) (*dynamicpb.Message, error) {
	mt, err := r.FindMessageByName(name)
	if err != nil {
		return nil, err
	}
	return dynamicpb.NewMessage(mt.Descriptor()), nil
}

// RangeMessages iterates every message type currently visible to the
// Resolver — both the bound namespace's own types and types
// contributed by parent registries. f is invoked once per type;
// returning false stops the walk.
//
// Tiers walked:
//   - The bound namespace's nsTypes (always).
//   - When [WithServerChain] was used: each ancestor's nsTypes, in
//     chain order (nearest first).
//   - When [WithParent] / [WithFallback] / [WithGlobalFallback] was
//     used without WithServerChain: the single parent tier supplied
//     at construction. Recursive multi-tier walking through ad-hoc
//     parents isn't supported — WithServerChain is the way to enumerate
//     a full org hierarchy.
//
// f may be called with the same FQN twice if two tiers both export it
// (a child intentionally shadowing a parent). Consumers building a
// deduplicated list should track names as they observe them.
//
// Useful for editor integrations populating a completion list of
// known message FQNs (e.g. for the `@type` directive in a PXF
// document). The walk runs against the resolver's current snapshot;
// concurrent refreshes do not introduce torn views.
func (r *Resolver) RangeMessages(f func(protoreflect.MessageType) bool) {
	if r == nil {
		return
	}
	cont := true
	walk := func(types *protoregistry.NamespacedTypes) {
		if !cont || types == nil {
			return
		}
		types.RangeMessages(func(mt protoreflect.MessageType) bool {
			if !f(mt) {
				cont = false
				return false
			}
			return true
		})
	}

	walk(r.nsTypes)
	for _, a := range r.ancestors {
		walk(a.nsTypes)
	}
	if len(r.ancestors) == 0 {
		walk(r.cfg.parentTypes)
	}
}

// FindMessageByNameWithOrigin is like [Resolver.FindMessageByName] but
// also returns the ID of the namespace that contributed the type.
// Useful for editor integrations rendering provenance ("defined in
// namespace acme-shared") next to hover or completion results.
//
// Origin semantics:
//   - The bound namespace's local types resolve with origin == r.Namespace().
//   - When [WithServerChain] is in effect, each ancestor tier resolves with
//     its own namespace ID — walked nearest-first.
//   - When [WithParent] / [WithFallback] / [WithGlobalFallback] is in effect
//     (no WithServerChain), the parent tier resolves the type but origin
//     is the empty string — the ad-hoc parent passes only registries, not
//     namespace identity. Use WithServerChain for full provenance.
//
// On NotFound, returns ("", "", NotFound) — both return values are
// zero so callers can ignore the origin without a nil-check.
func (r *Resolver) FindMessageByNameWithOrigin(name protoreflect.FullName) (protoreflect.MessageType, string, error) {
	if r == nil {
		return nil, "", protoregistry.NotFound
	}
	if mt := findLocalMessage(r.nsTypes, name); mt != nil {
		return mt, r.ns, nil
	}
	for _, a := range r.ancestors {
		if mt := findLocalMessage(a.nsTypes, name); mt != nil {
			return mt, a.ns, nil
		}
	}
	if len(r.ancestors) == 0 && r.cfg.parentTypes != nil {
		if mt, err := r.cfg.parentTypes.FindMessageByName(name); err == nil {
			return mt, "", nil
		}
	}
	return nil, "", protoregistry.NotFound
}

// FindFileByPathWithOrigin is like [Resolver.FindFileByPath] but also
// returns the ID of the namespace that contributed the file. Same
// origin semantics as [FindMessageByNameWithOrigin].
//
// Typical use: protolsp's hover handler resolves a field's
// ParentFile().Path() through this method to label the hover with
// "defined in namespace X" alongside the bare file path.
func (r *Resolver) FindFileByPathWithOrigin(path string) (protoreflect.FileDescriptor, string, error) {
	if r == nil {
		return nil, "", protoregistry.NotFound
	}
	if fd := findLocalFile(r.nsFiles, path); fd != nil {
		return fd, r.ns, nil
	}
	for _, a := range r.ancestors {
		if fd := findLocalFile(a.nsFiles, path); fd != nil {
			return fd, a.ns, nil
		}
	}
	if len(r.ancestors) == 0 && r.cfg.parentFiles != nil {
		if fd, err := r.cfg.parentFiles.FindFileByPath(path); err == nil {
			return fd, "", nil
		}
	}
	return nil, "", protoregistry.NotFound
}

// GetSource fetches the original .proto source bytes for the given
// file path. The path matches what a [protoreflect.FileDescriptor]
// reports via Path() — relative within the schema (e.g.
// "acme/billing/v1/invoice.proto").
//
// Resolution walks the same tiers as [Resolver.FindFileByPathWithOrigin]:
//   - Bound namespace's schemas first.
//   - With [WithServerChain]: each ancestor's schemas, nearest first.
//
// [WithParent] / [WithFallback] / [WithGlobalFallback] tiers expose
// only pre-compiled registries, not a registry connection — files
// reachable only through such ad-hoc parents resolve via
// FindFileByPath but return NotFound from GetSource. Use
// WithServerChain to get source-fetch coverage across the whole
// org's chain.
//
// The fetched version matches the currently-loaded snapshot — for a
// pinned Resolver, the pin's version; for a live one, the latest
// observed. The server is hit on every call; the client does not
// cache source bytes. Editor integrations should cache at their
// layer, keyed by the FileDescriptor's identity.
//
// Useful for editor integrations building virtual documents for
// registry-only files: when go-to-definition resolves to a file
// that doesn't exist on disk, the editor fetches its bytes here.
func (r *Resolver) GetSource(ctx context.Context, filePath string) ([]byte, error) {
	if r == nil {
		return nil, protoregistry.NotFound
	}
	if content, ok, err := r.fetchOwnSource(ctx, filePath); err != nil {
		return nil, err
	} else if ok {
		return content, nil
	}
	for _, a := range r.ancestors {
		if content, ok, err := a.fetchOwnSource(ctx, filePath); err != nil {
			return nil, err
		} else if ok {
			return content, nil
		}
	}
	return nil, protoregistry.NotFound
}

// fetchOwnSource locates filePath among the Resolver's own local
// schemas (no ancestor walk) and fetches its source bytes via the
// gRPC GetSource RPC. Returns (nil, false, nil) when the path isn't
// in any local schema, (content, true, nil) on success, and a
// non-nil error only when the server lookup itself fails.
//
// Cross-schema duplicate paths (rare; the registry should prevent
// them) resolve to whichever schema is encountered first — which
// may differ from [FindFileByPath]'s last-write-wins. Documenting
// rather than fixing: deduplication belongs at publish time.
func (r *Resolver) fetchOwnSource(ctx context.Context, filePath string) ([]byte, bool, error) {
	snap := r.snapshot.Load()
	if snap == nil {
		return nil, false, nil
	}
	var owner *schemaSnapshot
	for _, ss := range snap.schemas {
		for _, p := range ss.aggFingerprint.filePaths {
			if p == filePath {
				owner = ss
				break
			}
		}
		if owner != nil {
			break
		}
	}
	if owner == nil {
		return nil, false, nil
	}
	resp, err := r.rpc.GetSource(ctx, &registrypb.GetSourceRequest{
		NamespaceId: r.ns,
		SchemaId:    owner.schemaID,
		Version:     owner.version,
	})
	if err != nil {
		return nil, false, fmt.Errorf("get_source %s/%s@%d: %w", r.ns, owner.schemaID, owner.version, err)
	}
	content, ok := resp.Sources[filePath]
	if !ok {
		return nil, false, fmt.Errorf("server returned schema %s/%s@%d sources without %q: %w",
			r.ns, owner.schemaID, owner.version, filePath, protoregistry.NotFound)
	}
	return content, true, nil
}

// findLocalMessage scans only types registered directly on t (no
// parent-chain walk) for the given full name. nsTypes.RangeMessages
// is local-only by design — the parent-walking lookup methods don't
// expose tier identity, so we iterate to get it.
func findLocalMessage(t *protoregistry.NamespacedTypes, name protoreflect.FullName) protoreflect.MessageType {
	if t == nil {
		return nil
	}
	var found protoreflect.MessageType
	t.RangeMessages(func(mt protoreflect.MessageType) bool {
		if mt.Descriptor().FullName() == name {
			found = mt
			return false
		}
		return true
	})
	return found
}

// findLocalFile is the file-tier counterpart to findLocalMessage.
func findLocalFile(f *protoregistry.NamespacedFiles, path string) protoreflect.FileDescriptor {
	if f == nil {
		return nil
	}
	var found protoreflect.FileDescriptor
	f.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if fd.Path() == path {
			found = fd
			return false
		}
		return true
	})
	return found
}

// --- SchemaResolver ---

// FindMessageByName looks up a message type within the bound schema.
func (s *SchemaResolver) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	if s == nil || s.parent == nil {
		return nil, protoregistry.NotFound
	}
	snap := s.parent.snapshot.Load()
	if snap == nil {
		return nil, protoregistry.NotFound
	}
	ss, ok := snap.schemas[s.schemaID]
	if !ok {
		return nil, fmt.Errorf("schema %s not present in namespace %s: %w", s.schemaID, s.parent.ns, protoregistry.NotFound)
	}
	return ss.types.FindMessageByName(name)
}

// NewMessage constructs an empty dynamic message from the bound schema.
func (s *SchemaResolver) NewMessage(name protoreflect.FullName) (*dynamicpb.Message, error) {
	mt, err := s.FindMessageByName(name)
	if err != nil {
		return nil, err
	}
	return dynamicpb.NewMessage(mt.Descriptor()), nil
}

// SchemaID returns the schema this resolver is scoped to.
func (s *SchemaResolver) SchemaID() string {
	if s == nil {
		return ""
	}
	return s.schemaID
}

// --- Options ---

// Option configures a Resolver at construction time.
type Option func(*config)

type config struct {
	refresh time.Duration
	schemas []string
	logger  *slog.Logger
	token   string // only honored by Dial; New callers configure auth on the conn

	// transportCreds, when non-nil, replaces Dial's default
	// insecure.NewCredentials() with the supplied gRPC transport
	// credentials. Only honored by Dial; New callers configure
	// credentials on the *grpc.ClientConn directly. See
	// [WithTransportCredentials].
	transportCreds credentials.TransportCredentials

	// useServerChain, when true, makes New consult GetNamespaceChain on
	// startup and construct ancestor resolvers automatically, wiring the
	// nearest ancestor's nsFiles/nsTypes as parentFiles/parentTypes.
	// Mutually-exclusive in practice with WithParent/WithFallback/
	// WithGlobalFallback (the server-derived chain overwrites whatever
	// parent was previously configured — last writer wins among options).
	useServerChain bool

	// Optional parent registries for hierarchical fallback. When set,
	// the namespace-wide nsFiles / nsTypes and each per-schema
	// NamespacedFiles / NamespacedTypes are constructed as children of
	// these parents — local lookups take precedence; misses fall
	// through to the parent. See WithFallback / WithParent /
	// WithGlobalFallback.
	parentFiles *protoregistry.NamespacedFiles
	parentTypes *protoregistry.NamespacedTypes

	// cacheDir, when non-empty, enables on-disk descriptor caching.
	// The Resolver persists each successful refresh under
	// <cacheDir>/<namespace>/ ; [Dial] falls back to loading that
	// snapshot when the server is unreachable. See [WithDiskCache]
	// for the full contract.
	cacheDir string
}

func defaultConfig() config {
	return config{
		refresh: DefaultRefreshInterval,
	}
}

// WithRefreshInterval sets the polling cadence for current-version
// changes. Passing 0 disables refresh entirely (the Resolver becomes
// effectively pinned to its initial population).
//
// Default: [DefaultRefreshInterval].
func WithRefreshInterval(d time.Duration) Option {
	return func(c *config) { c.refresh = d }
}

// WithSchemas restricts the Resolver to a subset of schemas in the
// namespace. Useful when a service only consumes a known set of types
// and wants to skip fetching the rest.
//
// When unset, the Resolver tracks every schema in the namespace.
func WithSchemas(ids ...string) Option {
	return func(c *config) { c.schemas = append([]string(nil), ids...) }
}

// WithLogger sets a structured logger for refresh activity, cache swaps,
// and stale-while-error events. Nil falls back to [slog.Default]; pass
// a discard logger to silence output.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) { c.logger = l }
}

// WithToken attaches a bearer token to outgoing requests. Only honored
// by [Dial]; callers of [New] should configure auth on the
// *grpc.ClientConn directly (e.g. via grpc.WithPerRPCCredentials), which
// is more flexible and idiomatic.
func WithToken(token string) Option {
	return func(c *config) { c.token = token }
}

// WithTransportCredentials supplies gRPC transport credentials for
// [Dial]. The default — no option set — is insecure transport, fine
// for loopback / local development and never appropriate for traffic
// that leaves the host. Production callers pass
// credentials.NewTLS(tlsCfg) (or any other TransportCredentials
// implementation) to enable TLS while preserving Dial's
// cache-fallback behavior on the failure path.
//
// Only honored by [Dial]. Callers of [New] own the *grpc.ClientConn
// and configure credentials on it directly — this option is ignored
// in that path.
func WithTransportCredentials(creds credentials.TransportCredentials) Option {
	return func(c *config) { c.transportCreds = creds }
}

// WithFallback configures parent registries that the Resolver falls
// back to when a local lookup misses. The Resolver's namespace-wide
// aggregate (FindFileByPath / FindExtensionByNumber) and each
// per-schema view (Schema(...) lookups) both inherit the same parent,
// so well-known or shared types are visible at every lookup tier.
//
// Parent registries are read-only from the Resolver's perspective; the
// Resolver never writes to them, so callers manage their lifecycle.
// Passing the same pair to multiple Resolvers shares the parent across
// namespaces.
//
// Calling WithFallback twice — or combining it with [WithParent] /
// [WithGlobalFallback] — overrides the previous setting (last writer
// wins).
//
// Note: for namespace-hierarchy use cases (org-shared types living in
// a parent namespace on the server), prefer [WithServerChain] — it
// consults the registry's authoritative chain, so client and server
// can't disagree about what types are visible.
func WithFallback(files *protoregistry.NamespacedFiles, types *protoregistry.NamespacedTypes) Option {
	return func(c *config) {
		c.parentFiles = files
		c.parentTypes = types
	}
}

// WithParent makes this Resolver fall back to another Resolver's
// namespace-wide aggregate when local lookups miss. Useful for
// modeling a "common types" namespace as the parent of per-tenant
// namespaces — the parent Resolver continues to refresh independently
// and the child sees its current state via the fork's fallback chain.
//
// The parent must outlive every child. Closing the parent does not
// invalidate the child's fallback chain — operations after the parent
// is closed will still attempt to read its registries — so call sites
// should be careful with lifecycle ordering.
//
// Equivalent to calling [WithFallback] with the parent's nsFiles /
// nsTypes.
//
// Note: for namespace-hierarchy use cases (the parent namespace lives
// on the server as the registry-known ancestor), prefer
// [WithServerChain] — it sources the chain from the registry rather
// than the client, eliminating drift.
func WithParent(parent *Resolver) Option {
	return func(c *config) {
		if parent == nil {
			return
		}
		c.parentFiles = parent.nsFiles
		c.parentTypes = parent.nsTypes
	}
}

// WithGlobalFallback configures the Resolver to fall back to upstream
// [protoregistry.GlobalFiles] / [protoregistry.GlobalTypes] when a
// lookup misses. Useful when the binary also has generated proto
// types compiled in (which auto-register into the globals at init
// time); the Resolver can then resolve both registry-managed and
// statically-known types through the same lookup paths.
//
// The globals are read-only through this fallback — the Resolver
// never writes to them.
//
// Equivalent to calling [WithFallback] with a pair of global-wrapping
// registries derived from [protoregistry.NewNamespaceOverGlobal].
//
// Note: for namespace-hierarchy use cases (org-shared types living in
// a parent namespace on the server), prefer [WithServerChain] — it
// consults the registry's authoritative chain rather than relying on
// client-side configuration that could drift.
func WithGlobalFallback() Option {
	return func(c *config) {
		ns := protoregistry.NewNamespaceOverGlobal()
		c.parentFiles = ns.Files
		c.parentTypes = ns.Types
	}
}

// WithServerChain makes the Resolver consult the registry's
// GetNamespaceChain RPC at construction time and auto-configure
// ancestor Resolvers as parents. Each ancestor in the chain (parent,
// grandparent, …) is loaded as its own Resolver sharing the same gRPC
// connection, with its own refresh goroutine; the nearest ancestor's
// namespace-wide registries become the immediate parent of this
// Resolver.
//
// This is the recommended way to consume org-shared types: the
// registry is the single source of truth for which namespaces are in
// the chain, so the client cannot disagree with the server about what
// types are visible.
//
// Combining WithServerChain with [WithParent] / [WithFallback] /
// [WithGlobalFallback] is "last writer wins" on the parentFiles /
// parentTypes pair — if WithServerChain is the last option applied,
// the server chain overwrites whatever was previously configured. In
// practice the two patterns serve different use cases (server-derived
// org chain vs. ad-hoc test/library scenarios) and shouldn't be
// combined.
//
// The constructed ancestor Resolvers are closed when the main
// Resolver's Close is called; refresh goroutines stop cleanly.
func WithServerChain() Option {
	return func(c *config) { c.useServerChain = true }
}

// --- internal helpers ---

func resolveLogger(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.Default()
}

func (r *Resolver) tracksSchema(id string) bool {
	if len(r.cfg.schemas) == 0 {
		return true
	}
	for _, s := range r.cfg.schemas {
		if s == id {
			return true
		}
	}
	return false
}

// bearerCreds attaches an Authorization: Bearer header to every RPC.
// RequireTransportSecurity returns false to allow insecure dev setups —
// production callers should use New with their own conn instead.
type bearerCreds struct{ token string }

func (b bearerCreds) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (bearerCreds) RequireTransportSecurity() bool { return false }
