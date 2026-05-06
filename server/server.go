// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package server implements the gRPC RegistryService.
//
// The server is the public entry point to a running protoregistry. It
// validates and rate-limits incoming requests, applies the configured
// Authenticator (defaulting to NoAuth), gates privileged operations
// (writes to the __builtins__ namespace, Publish with Force=true) on
// Identity.Admin, and sanitizes errors so that backend implementation
// detail (raw PostgreSQL errors, unwrapped wrap chains) does not leak to
// clients.
package server

import (
	"context"
	"encoding/base64"
	"log/slog"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	protoregistry "github.com/trendvidia/protoregistry"
	registrypb "github.com/trendvidia/protoregistry/proto/protoregistry/v1"
	"github.com/trendvidia/protoregistry/store"
)

// Options configures a Server. Use the With* helpers rather than mutating
// directly; the zero Options is not a valid configuration.
type Options struct {
	// Limits caps incoming RPC payload sizes. Defaults to DefaultLimits().
	Limits Limits

	// Auth runs on every RPC and produces the Identity on the request
	// context. Defaults to NoAuth (anonymous, non-admin).
	Auth Authenticator

	// Logger receives audit and warning lines. Defaults to slog.Default().
	Logger *slog.Logger

	// AllowAnonymousWrites controls whether write RPCs (Publish, Promote,
	// Rollback, DiscardStaging, CreateNamespace) accept callers whose
	// Identity is AnonymousIdentity. Defaults to true to preserve the
	// pre-auth-seam behaviour; operators who want to lock the server down
	// should set this to false and configure WithAuth.
	AllowAnonymousWrites bool
}

// DefaultOptions returns the Options used when no With* options are passed
// to New. The defaults are permissive enough to keep existing local-dev
// setups working (anonymous reads, anonymous writes, no auth) but the
// server emits a startup warning in this configuration so the operator
// knows it is unprotected.
func DefaultOptions() Options {
	return Options{
		Limits:               DefaultLimits(),
		Auth:                 NoAuth{},
		Logger:               slog.Default(),
		AllowAnonymousWrites: true,
	}
}

// Option mutates an Options. Apply via New(reg, store, WithAuth(...), ...).
type Option func(*Options)

// WithLimits replaces the default Limits.
func WithLimits(l Limits) Option { return func(o *Options) { o.Limits = l } }

// WithAuth installs an Authenticator. The Authenticator's result is
// available on the request context via IdentityFromContext.
func WithAuth(a Authenticator) Option { return func(o *Options) { o.Auth = a } }

// WithLogger replaces the default slog.Logger used for audit lines.
func WithLogger(l *slog.Logger) Option { return func(o *Options) { o.Logger = l } }

// WithAllowAnonymousWrites controls whether anonymous callers may invoke
// write RPCs. The default is true; pass false to require authentication
// for any state-mutating call.
func WithAllowAnonymousWrites(allow bool) Option {
	return func(o *Options) { o.AllowAnonymousWrites = allow }
}

// Server implements the RegistryService gRPC server.
type Server struct {
	registrypb.UnimplementedRegistryServiceServer

	registry *protoregistry.Registry
	store    store.Store
	opts     Options
}

// New creates a new gRPC server backed by the given registry and store.
// The variadic options replace fields on DefaultOptions.
func New(registry *protoregistry.Registry, st store.Store, opts ...Option) *Server {
	o := DefaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Auth == nil {
		o.Auth = NoAuth{}
	}
	return &Server{
		registry: registry,
		store:    st,
		opts:     o,
	}
}

// requireWriter rejects write RPCs from anonymous callers when
// AllowAnonymousWrites is false. Authenticated callers (any non-anonymous
// Identity) always pass.
func (s *Server) requireWriter(id Identity) error {
	if id.Subject == "" || id == AnonymousIdentity {
		if !s.opts.AllowAnonymousWrites {
			return status.Error(codes.PermissionDenied,
				"write operations require authentication")
		}
	}
	return nil
}

// requireAdmin rejects callers without Identity.Admin set. Used to gate
// operations that can affect every namespace (writes to __builtins__) or
// that bypass safety checks (Publish with Force=true).
func (s *Server) requireAdmin(id Identity, op string) error {
	if id.Admin {
		return nil
	}
	return status.Errorf(codes.PermissionDenied, "%s requires admin", op)
}

// internalError logs the full error server-side and returns a sanitized
// gRPC status. The caller passes a short op string so the log line is
// greppable; we deliberately do not include the wrapped error in the
// returned status to avoid leaking backend detail (PostgreSQL error
// codes, table names, file paths) to gRPC clients.
//
// Errors that have already been wrapped in a gRPC status (e.g. by
// validateID) pass through unchanged.
func (s *Server) internalError(ctx context.Context, op string, err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	id := IdentityFromContext(ctx)
	s.opts.Logger.ErrorContext(ctx, "rpc failed",
		"op", op,
		"subject", id.Subject,
		"err", err)
	return status.Errorf(codes.Internal, "%s failed", op)
}

// audit emits a structured INFO line for a successful state-mutating RPC.
// Keep the field set lean and stable: this is the audit trail that
// operators will grep, parse, and ship to a SIEM. Identity is always
// included so anonymous and authenticated activity stay distinguishable.
func (s *Server) audit(ctx context.Context, op string, fields ...any) {
	id := IdentityFromContext(ctx)
	all := make([]any, 0, len(fields)+4)
	all = append(all, "op", op, "subject", id.Subject)
	all = append(all, fields...)
	s.opts.Logger.InfoContext(ctx, "audit", all...)
}

// --- Handlers ---

func (s *Server) Publish(ctx context.Context, req *registrypb.PublishRequest) (*registrypb.PublishResponse, error) {
	id := IdentityFromContext(ctx)
	if err := s.requireWriter(id); err != nil {
		return nil, err
	}
	if err := s.validateNamespaceID(req.NamespaceId, id); err != nil {
		return nil, err
	}
	if err := validateID(req.SchemaId, "schema_id", s.opts.Limits.MaxIDLength); err != nil {
		return nil, err
	}
	if err := validateSources(req.Sources, s.opts.Limits); err != nil {
		return nil, err
	}
	if req.NamespaceId == protoregistry.BuiltinsNamespace {
		if err := s.requireAdmin(id, "writing to __builtins__"); err != nil {
			return nil, err
		}
	}
	if req.Force {
		if err := s.requireAdmin(id, "force=true"); err != nil {
			return nil, err
		}
		s.opts.Logger.WarnContext(ctx, "force publish",
			"subject", id.Subject,
			"namespace", req.NamespaceId,
			"schema", req.SchemaId)
	}

	result, err := s.registry.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: req.NamespaceId,
		SchemaID:    req.SchemaId,
		Sources:     req.Sources,
		CreatedBy:   req.CreatedBy,
		Metadata:    req.Metadata,
		Force:       req.Force,
	})
	if err != nil {
		return nil, s.internalError(ctx, "publish", err)
	}

	s.audit(ctx, "publish",
		"namespace", req.NamespaceId,
		"schema", req.SchemaId,
		"version", result.Version,
		"no_change", result.NoChange,
		"force", req.Force,
		"files", len(req.Sources),
	)

	return &registrypb.PublishResponse{
		Version:     result.Version,
		Fingerprint: result.Fingerprint,
		NoChange:    result.NoChange,
	}, nil
}

func (s *Server) Promote(ctx context.Context, req *registrypb.PromoteRequest) (*registrypb.PromoteResponse, error) {
	id := IdentityFromContext(ctx)
	if err := s.requireWriter(id); err != nil {
		return nil, err
	}
	if err := s.validateNamespaceID(req.NamespaceId, id); err != nil {
		return nil, err
	}

	result, err := s.registry.Promote(ctx, req.NamespaceId)
	if err != nil {
		// Compat-check failures from the registry are preconditions, not
		// internal errors — surface them directly so callers can reason.
		if isCompatError(err) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, s.internalError(ctx, "promote", err)
	}

	promoted := make([]*registrypb.PromotedSchema, len(result.Promoted))
	for i, p := range result.Promoted {
		promoted[i] = &registrypb.PromotedSchema{
			SchemaId:       p.SchemaID,
			CurrentVersion: p.CurrentVersion,
		}
	}
	s.audit(ctx, "promote",
		"namespace", req.NamespaceId,
		"promoted_count", len(promoted),
	)
	return &registrypb.PromoteResponse{Promoted: promoted}, nil
}

func (s *Server) DiscardStaging(ctx context.Context, req *registrypb.DiscardStagingRequest) (*registrypb.DiscardStagingResponse, error) {
	id := IdentityFromContext(ctx)
	if err := s.requireWriter(id); err != nil {
		return nil, err
	}
	if err := s.validateNamespaceID(req.NamespaceId, id); err != nil {
		return nil, err
	}

	if err := s.registry.DiscardStaging(ctx, req.NamespaceId); err != nil {
		return nil, s.internalError(ctx, "discard_staging", err)
	}
	s.audit(ctx, "discard_staging", "namespace", req.NamespaceId)
	return &registrypb.DiscardStagingResponse{}, nil
}

func (s *Server) Rollback(ctx context.Context, req *registrypb.RollbackRequest) (*registrypb.RollbackResponse, error) {
	id := IdentityFromContext(ctx)
	if err := s.requireWriter(id); err != nil {
		return nil, err
	}
	if err := s.validateNamespaceID(req.NamespaceId, id); err != nil {
		return nil, err
	}
	if err := validateID(req.SchemaId, "schema_id", s.opts.Limits.MaxIDLength); err != nil {
		return nil, err
	}
	if req.Version == 0 {
		return nil, status.Error(codes.InvalidArgument, "version is required")
	}
	if req.Force {
		if err := s.requireAdmin(id, "force=true"); err != nil {
			return nil, err
		}
		s.opts.Logger.WarnContext(ctx, "force rollback",
			"subject", id.Subject,
			"namespace", req.NamespaceId,
			"schema", req.SchemaId,
			"version", req.Version)
	}

	err := s.registry.Rollback(ctx, req.NamespaceId, req.SchemaId, req.Version,
		protoregistry.RollbackOptions{Force: req.Force})
	if err != nil {
		// "would break consumers" is a precondition failure, not internal.
		if strings.Contains(err.Error(), "would break consumers") {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, s.internalError(ctx, "rollback", err)
	}
	s.audit(ctx, "rollback",
		"namespace", req.NamespaceId,
		"schema", req.SchemaId,
		"version", req.Version,
		"force", req.Force,
	)
	return &registrypb.RollbackResponse{}, nil
}

func (s *Server) GetSchema(ctx context.Context, req *registrypb.GetSchemaRequest) (*registrypb.GetSchemaResponse, error) {
	id := IdentityFromContext(ctx)
	if err := s.validateNamespaceID(req.NamespaceId, id); err != nil {
		return nil, err
	}
	if err := validateID(req.SchemaId, "schema_id", s.opts.Limits.MaxIDLength); err != nil {
		return nil, err
	}

	schema, err := s.store.GetSchema(ctx, req.NamespaceId, req.SchemaId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "schema %s/%s not found", req.NamespaceId, req.SchemaId)
	}

	versions, err := s.store.ListVersions(ctx, req.NamespaceId, req.SchemaId)
	if err != nil {
		return nil, s.internalError(ctx, "get_schema_versions", err)
	}

	return &registrypb.GetSchemaResponse{
		Schema: schemaToProto(schema, versions),
	}, nil
}

func (s *Server) ListSchemas(ctx context.Context, req *registrypb.ListSchemasRequest) (*registrypb.ListSchemasResponse, error) {
	id := IdentityFromContext(ctx)
	if err := s.validateNamespaceID(req.NamespaceId, id); err != nil {
		return nil, err
	}
	pageSize := s.opts.Limits.resolvePageSize(req.PageSize)
	after, err := decodePageToken(req.PageToken)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page_token")
	}
	if len(after) > s.opts.Limits.MaxIDLength {
		return nil, status.Error(codes.InvalidArgument, "page_token cursor exceeds id length limit")
	}

	// Fetch one extra row to detect whether more pages exist without a
	// separate COUNT query.
	schemas, err := s.store.ListSchemasPage(ctx, req.NamespaceId, after, pageSize+1)
	if err != nil {
		return nil, s.internalError(ctx, "list_schemas", err)
	}

	var nextToken string
	if len(schemas) > pageSize {
		schemas = schemas[:pageSize]
		nextToken = encodePageToken(schemas[len(schemas)-1].SchemaID)
	}

	out := make([]*registrypb.SchemaInfo, len(schemas))
	for i, schema := range schemas {
		out[i] = schemaToProto(schema, nil)
	}
	return &registrypb.ListSchemasResponse{
		Schemas:       out,
		NextPageToken: nextToken,
	}, nil
}

func (s *Server) GetDescriptor(ctx context.Context, req *registrypb.GetDescriptorRequest) (*registrypb.GetDescriptorResponse, error) {
	id := IdentityFromContext(ctx)
	if err := s.validateNamespaceID(req.NamespaceId, id); err != nil {
		return nil, err
	}
	if err := validateID(req.SchemaId, "schema_id", s.opts.Limits.MaxIDLength); err != nil {
		return nil, err
	}

	version := req.Version
	if version == 0 {
		snap := s.registry.Current(req.NamespaceId, req.SchemaId)
		if snap == nil {
			return nil, status.Error(codes.NotFound, "no current version")
		}
		version = snap.Version()
	}

	ver, err := s.store.GetVersion(ctx, req.NamespaceId, req.SchemaId, version)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "version %d not found", version)
	}

	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(ver.Compiled, fds); err != nil {
		return nil, s.internalError(ctx, "get_descriptor_unmarshal", err)
	}

	return &registrypb.GetDescriptorResponse{
		Version:           version,
		FileDescriptorSet: fds,
	}, nil
}

func (s *Server) GetSource(ctx context.Context, req *registrypb.GetSourceRequest) (*registrypb.GetSourceResponse, error) {
	id := IdentityFromContext(ctx)
	if err := s.validateNamespaceID(req.NamespaceId, id); err != nil {
		return nil, err
	}
	if err := validateID(req.SchemaId, "schema_id", s.opts.Limits.MaxIDLength); err != nil {
		return nil, err
	}

	version := req.Version
	if version == 0 {
		snap := s.registry.Current(req.NamespaceId, req.SchemaId)
		if snap == nil {
			return nil, status.Error(codes.NotFound, "no current version")
		}
		version = snap.Version()
	}

	files, err := s.store.GetVersionFiles(ctx, req.NamespaceId, req.SchemaId, version)
	if err != nil {
		return nil, s.internalError(ctx, "get_source_files", err)
	}

	sources := make(map[string][]byte, len(files))
	for _, f := range files {
		blob, err := s.store.GetBlob(ctx, req.NamespaceId, f.BlobSHA256)
		if err != nil {
			return nil, s.internalError(ctx, "get_source_blob", err)
		}
		sources[f.Filename] = blob.OriginalSource
	}

	return &registrypb.GetSourceResponse{
		Version: version,
		Sources: sources,
	}, nil
}

func (s *Server) ListNamespaces(ctx context.Context, req *registrypb.ListNamespacesRequest) (*registrypb.ListNamespacesResponse, error) {
	pageSize := s.opts.Limits.resolvePageSize(req.PageSize)
	after, err := decodePageToken(req.PageToken)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page_token")
	}
	if len(after) > s.opts.Limits.MaxIDLength {
		return nil, status.Error(codes.InvalidArgument, "page_token cursor exceeds id length limit")
	}

	namespaces, err := s.store.ListNamespacesPage(ctx, after, pageSize+1)
	if err != nil {
		return nil, s.internalError(ctx, "list_namespaces", err)
	}

	var nextToken string
	if len(namespaces) > pageSize {
		namespaces = namespaces[:pageSize]
		nextToken = encodePageToken(namespaces[len(namespaces)-1].ID)
	}

	out := make([]*registrypb.NamespaceInfo, len(namespaces))
	for i, ns := range namespaces {
		out[i] = namespaceToProto(ns)
	}
	return &registrypb.ListNamespacesResponse{
		Namespaces:    out,
		NextPageToken: nextToken,
	}, nil
}

func (s *Server) CreateNamespace(ctx context.Context, req *registrypb.CreateNamespaceRequest) (*registrypb.CreateNamespaceResponse, error) {
	id := IdentityFromContext(ctx)
	if err := s.requireWriter(id); err != nil {
		return nil, err
	}
	// Creating __builtins__ requires admin; the regexp would also reject
	// the literal because of the leading underscore, so check that case
	// first before validateID (which would mask the more specific error).
	if req.Id == protoregistry.BuiltinsNamespace {
		if err := s.requireAdmin(id, "creating __builtins__"); err != nil {
			return nil, err
		}
	} else if err := validateID(req.Id, "id", s.opts.Limits.MaxIDLength); err != nil {
		return nil, err
	}

	if err := s.store.CreateNamespace(ctx, &store.Namespace{
		ID:       req.Id,
		Metadata: req.Metadata,
	}); err != nil {
		return nil, s.internalError(ctx, "create_namespace", err)
	}
	s.audit(ctx, "create_namespace", "namespace", req.Id)
	return &registrypb.CreateNamespaceResponse{}, nil
}

// --- helpers ---

// validateNamespaceID validates a namespace ID, allowing the reserved
// __builtins__ identifier through the regexp gate. Reads of __builtins__
// are open; writes are gated separately by requireAdmin in the relevant
// handlers.
func (s *Server) validateNamespaceID(namespaceID string, _ Identity) error {
	if namespaceID == protoregistry.BuiltinsNamespace {
		return nil
	}
	return validateID(namespaceID, "namespace_id", s.opts.Limits.MaxIDLength)
}

// encodePageToken / decodePageToken use base64 so the cursor is opaque to
// callers (we may change the on-the-wire format later without breaking
// stored tokens). Today the payload is just the last seen ID.
func encodePageToken(cursor string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursor))
}

func decodePageToken(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// isCompatError matches the prefix used by registry.Promote when a
// staged-vs-current compat check rejects the promotion. String match
// because the registry layer does not yet expose a typed sentinel; when
// it does, switch this to errors.Is.
func isCompatError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "compatibility check failed")
}

func namespaceToProto(ns *store.Namespace) *registrypb.NamespaceInfo {
	return &registrypb.NamespaceInfo{
		Id:        ns.ID,
		CreatedAt: timestamppb.New(ns.CreatedAt),
		Metadata:  ns.Metadata,
	}
}

func schemaToProto(schema *store.Schema, versions []uint64) *registrypb.SchemaInfo {
	info := &registrypb.SchemaInfo{
		NamespaceId: schema.NamespaceID,
		SchemaId:    schema.SchemaID,
		CreatedAt:   timestamppb.New(schema.CreatedAt),
		Metadata:    schema.Metadata,
		Versions:    versions,
	}
	if schema.CurrentVersion != nil {
		cv := *schema.CurrentVersion
		info.CurrentVersion = &cv
	}
	if schema.StagedVersion != nil {
		sv := *schema.StagedVersion
		info.StagedVersion = &sv
	}
	return info
}

// Compile-time assertion that Server satisfies the generated interface.
var _ registrypb.RegistryServiceServer = (*Server)(nil)
