// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package store defines the persistence interface for the schema registry.
//
// Implementations are responsible for storing proto source blobs, schema
// version metadata, compiled descriptors, and dependency tracking. The
// interface is designed around content-addressable storage (proto blobs
// identified by SHA-256 of normalized content) with a separate versioning
// layer that references blobs by hash.
package store

import (
	"context"
	"time"
)

// Store is the persistence interface for the schema registry.
type Store interface {
	// Blob operations (content-addressable).

	// PutBlob stores a proto source blob. If a blob with the same namespace
	// and SHA-256 already exists, this is a no-op.
	PutBlob(ctx context.Context, blob *ProtoBlob) error

	// GetBlob retrieves a proto source blob by namespace and hash.
	GetBlob(ctx context.Context, namespaceID, sha256 string) (*ProtoBlob, error)

	// Namespace operations.

	// CreateNamespace creates a new namespace.
	CreateNamespace(ctx context.Context, ns *Namespace) error

	// GetNamespace retrieves a namespace by ID.
	GetNamespace(ctx context.Context, id string) (*Namespace, error)

	// ListNamespaces lists all non-deleted namespaces. Intended for in-process
	// callers that know the set is bounded (tests, embedded library use).
	// Server-side List RPCs use ListNamespacesPage instead.
	ListNamespaces(ctx context.Context) ([]*Namespace, error)

	// ListNamespacesPage returns up to limit namespaces whose ID is strictly
	// greater than after, ordered by ID. Pass an empty after to start at the
	// beginning. Keyset pagination — stable under concurrent writes.
	ListNamespacesPage(ctx context.Context, after string, limit int) ([]*Namespace, error)

	// Schema operations.

	// CreateSchema creates a new schema within a namespace.
	CreateSchema(ctx context.Context, s *Schema) error

	// GetSchema retrieves a schema by namespace and schema ID.
	GetSchema(ctx context.Context, namespaceID, schemaID string) (*Schema, error)

	// ListSchemas lists all schemas in a namespace. See ListNamespaces for the
	// distinction between this and the paginated variant below.
	ListSchemas(ctx context.Context, namespaceID string) ([]*Schema, error)

	// ListSchemasPage returns up to limit schemas in the namespace whose
	// schema ID is strictly greater than after, ordered by schema ID. Pass an
	// empty after to start at the beginning.
	ListSchemasPage(ctx context.Context, namespaceID, after string, limit int) ([]*Schema, error)

	// Version operations.

	// PutVersion stores a new schema version along with its file mappings
	// and dependency records. This is an atomic operation.
	PutVersion(ctx context.Context, ver *SchemaVersion, files []VersionFile, deps []VersionDep) error

	// GetVersion retrieves a specific schema version.
	GetVersion(ctx context.Context, namespaceID, schemaID string, version uint64) (*SchemaVersion, error)

	// GetVersionFiles retrieves the file mappings for a schema version.
	GetVersionFiles(ctx context.Context, namespaceID, schemaID string, version uint64) ([]VersionFile, error)

	// ListVersions lists all versions of a schema.
	ListVersions(ctx context.Context, namespaceID, schemaID string) ([]uint64, error)

	// Staging and promotion.

	// SetStaged sets the staged version for a schema. Pass 0 to clear staging.
	SetStaged(ctx context.Context, namespaceID, schemaID string, version uint64) error

	// Promote atomically moves all staged versions to current within a namespace.
	// Returns the schemas that were promoted.
	Promote(ctx context.Context, namespaceID string) ([]PromotedSchema, error)

	// DiscardStaging clears all staged versions in a namespace.
	DiscardStaging(ctx context.Context, namespaceID string) error

	// Recovery.

	// LoadAllCurrent loads all current schema versions across all namespaces.
	// Used at startup to rebuild in-memory state without recompilation.
	LoadAllCurrent(ctx context.Context) ([]*CurrentSchema, error)

	// LoadNamespaceCurrent loads all current schema versions for a single namespace.
	LoadNamespaceCurrent(ctx context.Context, namespaceID string) ([]*CurrentSchema, error)

	// LoadNamespaceProposed loads the proposed state for a namespace: staged
	// versions where they exist, current versions otherwise. Used to build
	// the resolver for compilation against the staging environment.
	LoadNamespaceProposed(ctx context.Context, namespaceID string) ([]*CurrentSchema, error)
}

// ProtoBlob is a content-addressable proto source file.
type ProtoBlob struct {
	NamespaceID    string
	SHA256         string
	OriginalSource []byte
	SizeBytes      int
	CreatedAt      time.Time
}

// Namespace is an isolation boundary for schemas.
type Namespace struct {
	ID        string
	CreatedAt time.Time
	DeletedAt *time.Time
	Metadata  map[string]string
}

// Schema is a named collection of proto files within a namespace.
type Schema struct {
	NamespaceID    string
	SchemaID       string
	CurrentVersion *uint64
	StagedVersion  *uint64
	CreatedAt      time.Time
	DeletedAt      *time.Time
	Metadata       map[string]string
}

// SchemaVersion is a specific version of a schema.
type SchemaVersion struct {
	NamespaceID     string
	SchemaID        string
	Version         uint64
	Compiled        []byte // serialized FileDescriptorSet
	CompilerVersion string
	CreatedAt       time.Time
	CreatedBy       string
	DeletedAt       *time.Time
	Metadata        map[string]string
}

// VersionFile links a schema version to a proto blob by filename and hash.
type VersionFile struct {
	NamespaceID string
	SchemaID    string
	Version     uint64
	Filename    string
	BlobSHA256  string
}

// VersionDep records that a schema version was compiled against a specific
// version of another schema's file.
type VersionDep struct {
	NamespaceID  string
	SchemaID     string
	Version      uint64
	DepSchemaID  string
	DepFilename  string
	DepVersion   uint64
}

// PromotedSchema describes a schema that was promoted from staged to current.
type PromotedSchema struct {
	SchemaID       string
	PriorVersion   *uint64
	CurrentVersion uint64
}

// CurrentSchema is the full state of a current schema version, including
// its file mappings. Used for startup recovery and building resolvers.
type CurrentSchema struct {
	NamespaceID     string
	SchemaID        string
	Version         uint64
	Compiled        []byte
	CompilerVersion string
	Files           []VersionFile
}
