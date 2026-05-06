// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package postgres implements the store.Store interface using PostgreSQL
// with sqlc-generated query code and pgx as the driver.
package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trendvidia/protoregistry/store"
	"github.com/trendvidia/protoregistry/store/postgres/sqlc"
)

// Store implements store.Store using PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
}

// New creates a new PostgreSQL store.
func New(pool *pgxpool.Pool) *Store {
	return &Store{
		pool: pool,
		q:    sqlc.New(pool),
	}
}

func (s *Store) CreateNamespace(ctx context.Context, ns *store.Namespace) error {
	return s.q.CreateNamespace(ctx, sqlc.CreateNamespaceParams{
		ID:       ns.ID,
		Metadata: marshalJSONOrEmpty(ns.Metadata),
	})
}

func (s *Store) GetNamespace(ctx context.Context, id string) (*store.Namespace, error) {
	row, err := s.q.GetNamespace(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("getting namespace %s: %w", id, err)
	}
	return &store.Namespace{
		ID:        row.ID,
		CreatedAt: row.CreatedAt,
		DeletedAt: pgtimeToPtr(row.DeletedAt),
		Metadata:  unmarshalJSONMap(row.Metadata),
	}, nil
}

func (s *Store) ListNamespaces(ctx context.Context) ([]*store.Namespace, error) {
	rows, err := s.q.ListNamespaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}
	result := make([]*store.Namespace, len(rows))
	for i, row := range rows {
		result[i] = &store.Namespace{
			ID:        row.ID,
			CreatedAt: row.CreatedAt,
			DeletedAt: pgtimeToPtr(row.DeletedAt),
			Metadata:  unmarshalJSONMap(row.Metadata),
		}
	}
	return result, nil
}

func (s *Store) ListNamespacesPage(ctx context.Context, after string, limit int) ([]*store.Namespace, error) {
	rows, err := s.q.ListNamespacesPage(ctx, sqlc.ListNamespacesPageParams{
		ID:    after,
		Limit: intToInt32Clamp(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces page: %w", err)
	}
	result := make([]*store.Namespace, len(rows))
	for i, row := range rows {
		result[i] = &store.Namespace{
			ID:        row.ID,
			CreatedAt: row.CreatedAt,
			DeletedAt: pgtimeToPtr(row.DeletedAt),
			Metadata:  unmarshalJSONMap(row.Metadata),
		}
	}
	return result, nil
}

func (s *Store) PutBlob(ctx context.Context, blob *store.ProtoBlob) error {
	return s.q.PutBlob(ctx, sqlc.PutBlobParams{
		NamespaceID:    blob.NamespaceID,
		Sha256:         blob.SHA256,
		OriginalSource: blob.OriginalSource,
		SizeBytes:      intToInt32Clamp(blob.SizeBytes),
	})
}

func (s *Store) GetBlob(ctx context.Context, namespaceID, sha256hex string) (*store.ProtoBlob, error) {
	row, err := s.q.GetBlob(ctx, sqlc.GetBlobParams{
		NamespaceID: namespaceID,
		Sha256:      sha256hex,
	})
	if err != nil {
		return nil, fmt.Errorf("getting blob %s/%s: %w", namespaceID, sha256hex, err)
	}
	return &store.ProtoBlob{
		NamespaceID:    row.NamespaceID,
		SHA256:         row.Sha256,
		OriginalSource: row.OriginalSource,
		SizeBytes:      int(row.SizeBytes),
		CreatedAt:      row.CreatedAt,
	}, nil
}

func (s *Store) CreateSchema(ctx context.Context, schema *store.Schema) error {
	return s.q.CreateSchema(ctx, sqlc.CreateSchemaParams{
		NamespaceID: schema.NamespaceID,
		SchemaID:    schema.SchemaID,
		Metadata:    marshalJSONOrEmpty(schema.Metadata),
	})
}

func (s *Store) GetSchema(ctx context.Context, namespaceID, schemaID string) (*store.Schema, error) {
	row, err := s.q.GetSchema(ctx, sqlc.GetSchemaParams{
		NamespaceID: namespaceID,
		SchemaID:    schemaID,
	})
	if err != nil {
		return nil, fmt.Errorf("getting schema %s/%s: %w", namespaceID, schemaID, err)
	}
	return schemaModelToStore(row), nil
}

func (s *Store) ListSchemas(ctx context.Context, namespaceID string) ([]*store.Schema, error) {
	rows, err := s.q.ListSchemas(ctx, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("listing schemas for %s: %w", namespaceID, err)
	}
	result := make([]*store.Schema, len(rows))
	for i, row := range rows {
		result[i] = schemaModelToStore(row)
	}
	return result, nil
}

func (s *Store) ListSchemasPage(ctx context.Context, namespaceID, after string, limit int) ([]*store.Schema, error) {
	rows, err := s.q.ListSchemasPage(ctx, sqlc.ListSchemasPageParams{
		NamespaceID: namespaceID,
		SchemaID:    after,
		Limit:       intToInt32Clamp(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("listing schemas page for %s: %w", namespaceID, err)
	}
	result := make([]*store.Schema, len(rows))
	for i, row := range rows {
		result[i] = schemaModelToStore(row)
	}
	return result, nil
}

func (s *Store) PutVersion(ctx context.Context, ver *store.SchemaVersion, files []store.VersionFile, deps []store.VersionDep) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := s.q.WithTx(tx)

	err = qtx.InsertVersion(ctx, sqlc.InsertVersionParams{
		NamespaceID:     ver.NamespaceID,
		SchemaID:        ver.SchemaID,
		Version:         versionToDB(ver.Version),
		Compiled:        ver.Compiled,
		CompilerVersion: ver.CompilerVersion,
		CreatedBy:       ver.CreatedBy,
		Metadata:        marshalJSONOrEmpty(ver.Metadata),
	})
	if err != nil {
		return fmt.Errorf("inserting version: %w", err)
	}

	for _, f := range files {
		err = qtx.InsertVersionFile(ctx, sqlc.InsertVersionFileParams{
			NamespaceID: f.NamespaceID,
			SchemaID:    f.SchemaID,
			Version:     versionToDB(f.Version),
			Filename:    f.Filename,
			BlobSha256:  f.BlobSHA256,
		})
		if err != nil {
			return fmt.Errorf("inserting version file %s: %w", f.Filename, err)
		}
	}

	for _, d := range deps {
		err = qtx.InsertVersionDep(ctx, sqlc.InsertVersionDepParams{
			NamespaceID: d.NamespaceID,
			SchemaID:    d.SchemaID,
			Version:     versionToDB(d.Version),
			DepSchemaID: d.DepSchemaID,
			DepFilename: d.DepFilename,
			DepVersion:  versionToDB(d.DepVersion),
		})
		if err != nil {
			return fmt.Errorf("inserting version dep: %w", err)
		}
	}

	return tx.Commit(ctx)
}

func (s *Store) GetVersion(ctx context.Context, namespaceID, schemaID string, version uint64) (*store.SchemaVersion, error) {
	row, err := s.q.GetVersion(ctx, sqlc.GetVersionParams{
		NamespaceID: namespaceID,
		SchemaID:    schemaID,
		Version:     versionToDB(version),
	})
	if err != nil {
		return nil, fmt.Errorf("getting version %s/%s@%d: %w", namespaceID, schemaID, version, err)
	}
	return &store.SchemaVersion{
		NamespaceID:     row.NamespaceID,
		SchemaID:        row.SchemaID,
		Version:         versionFromDB(row.Version),
		Compiled:        row.Compiled,
		CompilerVersion: row.CompilerVersion,
		CreatedAt:       row.CreatedAt,
		CreatedBy:       row.CreatedBy,
		DeletedAt:       pgtimeToPtr(row.DeletedAt),
		Metadata:        unmarshalJSONMap(row.Metadata),
	}, nil
}

func (s *Store) GetVersionFiles(ctx context.Context, namespaceID, schemaID string, version uint64) ([]store.VersionFile, error) {
	rows, err := s.q.GetVersionFiles(ctx, sqlc.GetVersionFilesParams{
		NamespaceID: namespaceID,
		SchemaID:    schemaID,
		Version:     versionToDB(version),
	})
	if err != nil {
		return nil, fmt.Errorf("getting version files: %w", err)
	}
	result := make([]store.VersionFile, len(rows))
	for i, row := range rows {
		result[i] = store.VersionFile{
			NamespaceID: row.NamespaceID,
			SchemaID:    row.SchemaID,
			Version:     versionFromDB(row.Version),
			Filename:    row.Filename,
			BlobSHA256:  row.BlobSha256,
		}
	}
	return result, nil
}

func (s *Store) ListVersions(ctx context.Context, namespaceID, schemaID string) ([]uint64, error) {
	rows, err := s.q.ListVersions(ctx, sqlc.ListVersionsParams{
		NamespaceID: namespaceID,
		SchemaID:    schemaID,
	})
	if err != nil {
		return nil, fmt.Errorf("listing versions: %w", err)
	}
	result := make([]uint64, len(rows))
	for i, v := range rows {
		result[i] = versionFromDB(v)
	}
	return result, nil
}

func (s *Store) SetStaged(ctx context.Context, namespaceID, schemaID string, version uint64) error {
	if version == 0 {
		return s.q.ClearStagedVersion(ctx, sqlc.ClearStagedVersionParams{
			NamespaceID: namespaceID,
			SchemaID:    schemaID,
		})
	}
	return s.q.SetStagedVersion(ctx, sqlc.SetStagedVersionParams{
		NamespaceID:   namespaceID,
		SchemaID:      schemaID,
		StagedVersion: pgint8(version),
	})
}

func (s *Store) Promote(ctx context.Context, namespaceID string) ([]store.PromotedSchema, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := s.q.WithTx(tx)

	// Lock staged schemas to serialize concurrent promotions.
	_, err = qtx.GetStagedSchemas(ctx, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("locking staged schemas: %w", err)
	}

	rows, err := qtx.PromoteAllStaged(ctx, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("promoting staged: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing promotion: %w", err)
	}

	result := make([]store.PromotedSchema, len(rows))
	for i, row := range rows {
		result[i] = store.PromotedSchema{
			SchemaID:       row.SchemaID,
			CurrentVersion: versionFromDB(row.CurrentVersion.Int64),
		}
	}
	return result, nil
}

func (s *Store) DiscardStaging(ctx context.Context, namespaceID string) error {
	return s.q.DiscardAllStaged(ctx, namespaceID)
}

func (s *Store) LoadAllCurrent(ctx context.Context) ([]*store.CurrentSchema, error) {
	rows, err := s.q.LoadAllCurrent(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading all current: %w", err)
	}
	return groupAllCurrentRows(rows), nil
}

func (s *Store) LoadNamespaceCurrent(ctx context.Context, namespaceID string) ([]*store.CurrentSchema, error) {
	rows, err := s.q.LoadNamespaceCurrent(ctx, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("loading namespace current: %w", err)
	}
	return groupNSCurrentRows(rows), nil
}

func (s *Store) LoadNamespaceProposed(ctx context.Context, namespaceID string) ([]*store.CurrentSchema, error) {
	rows, err := s.q.LoadNamespaceProposed(ctx, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("loading namespace proposed: %w", err)
	}
	return groupNSProposedRows(rows), nil
}

// BeginTx starts a transaction and returns queries scoped to it.
func (s *Store) BeginTx(ctx context.Context) (pgx.Tx, *sqlc.Queries, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	return tx, s.q.WithTx(tx), nil
}

// --- type conversion helpers ---

func schemaModelToStore(row sqlc.Schema) *store.Schema {
	return &store.Schema{
		NamespaceID:    row.NamespaceID,
		SchemaID:       row.SchemaID,
		CurrentVersion: pgint8ToPtr(row.CurrentVersion),
		StagedVersion:  pgint8ToPtr(row.StagedVersion),
		CreatedAt:      row.CreatedAt,
		DeletedAt:      pgtimeToPtr(row.DeletedAt),
		Metadata:       unmarshalJSONMap(row.Metadata),
	}
}

func groupAllCurrentRows(rows []sqlc.LoadAllCurrentRow) []*store.CurrentSchema {
	type key struct{ ns, schema string }
	index := make(map[key]*store.CurrentSchema)
	var result []*store.CurrentSchema

	for _, row := range rows {
		k := key{row.NamespaceID, row.SchemaID}
		cs, ok := index[k]
		if !ok {
			cs = &store.CurrentSchema{
				NamespaceID:     row.NamespaceID,
				SchemaID:        row.SchemaID,
				Version:         versionFromDB(row.Version),
				Compiled:        row.Compiled,
				CompilerVersion: row.CompilerVersion,
			}
			index[k] = cs
			result = append(result, cs)
		}
		cs.Files = append(cs.Files, store.VersionFile{
			NamespaceID: row.NamespaceID,
			SchemaID:    row.SchemaID,
			Version:     versionFromDB(row.Version),
			Filename:    row.Filename,
			BlobSHA256:  row.BlobSha256,
		})
	}
	return result
}

func groupNSCurrentRows(rows []sqlc.LoadNamespaceCurrentRow) []*store.CurrentSchema {
	type key struct{ ns, schema string }
	index := make(map[key]*store.CurrentSchema)
	var result []*store.CurrentSchema

	for _, row := range rows {
		k := key{row.NamespaceID, row.SchemaID}
		cs, ok := index[k]
		if !ok {
			cs = &store.CurrentSchema{
				NamespaceID:     row.NamespaceID,
				SchemaID:        row.SchemaID,
				Version:         versionFromDB(row.Version),
				Compiled:        row.Compiled,
				CompilerVersion: row.CompilerVersion,
			}
			index[k] = cs
			result = append(result, cs)
		}
		cs.Files = append(cs.Files, store.VersionFile{
			NamespaceID: row.NamespaceID,
			SchemaID:    row.SchemaID,
			Version:     versionFromDB(row.Version),
			Filename:    row.Filename,
			BlobSHA256:  row.BlobSha256,
		})
	}
	return result
}

func groupNSProposedRows(rows []sqlc.LoadNamespaceProposedRow) []*store.CurrentSchema {
	type key struct{ ns, schema string }
	index := make(map[key]*store.CurrentSchema)
	var result []*store.CurrentSchema

	for _, row := range rows {
		k := key{row.NamespaceID, row.SchemaID}
		cs, ok := index[k]
		if !ok {
			cs = &store.CurrentSchema{
				NamespaceID:     row.NamespaceID,
				SchemaID:        row.SchemaID,
				Version:         versionFromDB(row.Version),
				Compiled:        row.Compiled,
				CompilerVersion: row.CompilerVersion,
			}
			index[k] = cs
			result = append(result, cs)
		}
		cs.Files = append(cs.Files, store.VersionFile{
			NamespaceID: row.NamespaceID,
			SchemaID:    row.SchemaID,
			Version:     versionFromDB(row.Version),
			Filename:    row.Filename,
			BlobSHA256:  row.BlobSha256,
		})
	}
	return result
}

func pgint8(v uint64) pgtype.Int8 {
	return pgtype.Int8{Int64: versionToDB(v), Valid: true}
}

func pgint8ToPtr(v pgtype.Int8) *uint64 {
	if !v.Valid {
		return nil
	}
	u := versionFromDB(v.Int64)
	return &u
}

// versionToDB converts a wire-format uint64 schema version to the int64
// PostgreSQL BIGINT used in the schemas / files / deps tables. Versions
// are monotonic positive counters bounded well below 2^63 in practice;
// this helper concentrates the int-width crossing in one place so the
// gosec G115 suppression has a single, documented home.
func versionToDB(v uint64) int64 {
	if v > math.MaxInt64 {
		// Practically unreachable — versions never approach 2^63.
		// Clamp instead of overflowing into a negative value, which the
		// CHECK (version >= 0) constraint would reject anyway.
		return math.MaxInt64
	}
	return int64(v) // #nosec G115 -- bounds-checked above; versions are monotonic positive counters
}

// versionFromDB converts a non-negative DB int64 version to its uint64
// wire form. The schema enforces version >= 0; rows arriving with a
// negative value indicate corruption and we clamp to 0 rather than
// silently wrapping.
func versionFromDB(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v) // #nosec G115 -- bounds-checked above
}

// intToInt32Clamp narrows a Go int (used for paging limits and blob
// sizes) to int32, clamping out-of-range values to the column's
// representable range. Negative values clamp to zero; values above
// math.MaxInt32 clamp to math.MaxInt32. Callers that need exact
// fidelity should validate at the API boundary.
func intToInt32Clamp(n int) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < 0 {
		return 0
	}
	return int32(n) // #nosec G115 -- bounds-checked above
}

func pgtimeToPtr(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	return &v.Time
}

func marshalJSONOrEmpty(m map[string]string) []byte {
	if m == nil {
		return []byte("{}")
	}
	b, _ := json.Marshal(m)
	return b
}

func unmarshalJSONMap(b []byte) map[string]string {
	if len(b) == 0 {
		return nil
	}
	var m map[string]string
	_ = json.Unmarshal(b, &m)
	return m
}
