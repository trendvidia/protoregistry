// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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

	snapshot  atomic.Pointer[nsSnapshot]
	refreshMu sync.Mutex // serializes Refresh calls

	cancel context.CancelFunc
	wg     sync.WaitGroup
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

	r := &Resolver{
		conn:   conn,
		rpc:    registrypb.NewRegistryServiceClient(conn),
		ns:     namespace,
		cfg:    cfg,
		logger: resolveLogger(cfg.logger),
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
func Dial(ctx context.Context, addr, namespace string, opts ...Option) (*Resolver, error) {
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}

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

// Close stops the background refresh goroutine. If the Resolver was
// created via [Dial] it also closes the underlying gRPC connection;
// otherwise the conn was passed in by the caller and is left alone.
func (r *Resolver) Close() error {
	if r == nil {
		return nil
	}
	if r.cancel != nil {
		r.cancel()
		r.wg.Wait()
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

	pinned := &Resolver{
		conn:     r.conn,
		ownsConn: false,
		rpc:      r.rpc,
		ns:       r.ns,
		cfg:      r.cfg,
		logger:   r.logger,
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
	if err := snap.buildAggregates(); err != nil {
		return nil, err
	}
	pinned.snapshot.Store(snap)
	return pinned, nil
}

// --- protoregistry.MessageTypeResolver ---

// FindMessageByName looks up a message type by its fully-qualified name
// across all schemas in the namespace.
//
// Returns [protoregistry.NotFound] when no schema in the namespace
// defines the name.
func (r *Resolver) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	snap := r.snapshot.Load()
	if snap == nil {
		return nil, protoregistry.NotFound
	}
	ss, ok := snap.schemaFor(name)
	if !ok {
		return nil, protoregistry.NotFound
	}
	return ss.types.FindMessageByName(name)
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

// FindExtensionByName looks up an extension by its fully-qualified name.
func (r *Resolver) FindExtensionByName(name protoreflect.FullName) (protoreflect.ExtensionType, error) {
	snap := r.snapshot.Load()
	if snap == nil {
		return nil, protoregistry.NotFound
	}
	ss, ok := snap.schemaFor(name)
	if !ok {
		return nil, protoregistry.NotFound
	}
	return ss.types.FindExtensionByName(name)
}

// FindExtensionByNumber looks up an extension by the message it extends
// and its field number. Searches the namespace-wide aggregate built at
// snapshot construction time.
func (r *Resolver) FindExtensionByNumber(message protoreflect.FullName, field protoreflect.FieldNumber) (protoreflect.ExtensionType, error) {
	snap := r.snapshot.Load()
	if snap == nil {
		return nil, protoregistry.NotFound
	}
	return snap.nsTypes.FindExtensionByNumber(message, field)
}

// --- protodesc.Resolver ---

// FindFileByPath looks up a file descriptor by its proto path (e.g.
// "billing/v1/billing.proto"). Searches the namespace-wide aggregate
// built at snapshot construction time.
func (r *Resolver) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	snap := r.snapshot.Load()
	if snap == nil {
		return nil, protoregistry.NotFound
	}
	return snap.nsFiles.FindFileByPath(path)
}

// FindDescriptorByName looks up any descriptor (message, enum, service,
// extension, etc.) by its fully-qualified name.
func (r *Resolver) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	snap := r.snapshot.Load()
	if snap == nil {
		return nil, protoregistry.NotFound
	}
	ss, ok := snap.schemaFor(name)
	if !ok {
		return nil, protoregistry.NotFound
	}
	return ss.files.FindDescriptorByName(name)
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
