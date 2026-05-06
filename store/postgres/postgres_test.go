// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trendvidia/protoregistry/store"
	"github.com/trendvidia/protoregistry/store/postgres"
	"github.com/trendvidia/protoregistry/store/postgres/pgtest"
)

func setupStore(t *testing.T) (*postgres.Store, context.Context) {
	t.Helper()
	res := pgtest.Setup(t)
	return postgres.New(res.Pool), context.Background()
}

func TestNamespaceCRUD(t *testing.T) {
	s, ctx := setupStore(t)

	// Create.
	err := s.CreateNamespace(ctx, &store.Namespace{
		ID:       "acme",
		Metadata: map[string]string{"env": "test"},
	})
	require.NoError(t, err)

	// Get.
	ns, err := s.GetNamespace(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, "acme", ns.ID)
	assert.Equal(t, "test", ns.Metadata["env"])
	assert.Nil(t, ns.DeletedAt)

	// Duplicate create fails.
	err = s.CreateNamespace(ctx, &store.Namespace{ID: "acme"})
	require.Error(t, err)
}

func TestSchemaCRUD(t *testing.T) {
	s, ctx := setupStore(t)
	require.NoError(t, s.CreateNamespace(ctx, &store.Namespace{ID: "acme"}))

	// Create schema.
	err := s.CreateSchema(ctx, &store.Schema{
		NamespaceID: "acme",
		SchemaID:    "billing",
		Metadata:    map[string]string{"owner": "platform"},
	})
	require.NoError(t, err)

	// Get schema.
	schema, err := s.GetSchema(ctx, "acme", "billing")
	require.NoError(t, err)
	assert.Equal(t, "billing", schema.SchemaID)
	assert.Nil(t, schema.CurrentVersion)
	assert.Nil(t, schema.StagedVersion)
	assert.Equal(t, "platform", schema.Metadata["owner"])

	// List schemas.
	require.NoError(t, s.CreateSchema(ctx, &store.Schema{
		NamespaceID: "acme",
		SchemaID:    "common",
	}))
	schemas, err := s.ListSchemas(ctx, "acme")
	require.NoError(t, err)
	assert.Len(t, schemas, 2)
	assert.Equal(t, "billing", schemas[0].SchemaID)
	assert.Equal(t, "common", schemas[1].SchemaID)
}

func TestBlobPutAndGet(t *testing.T) {
	s, ctx := setupStore(t)
	require.NoError(t, s.CreateNamespace(ctx, &store.Namespace{ID: "acme"}))

	blob := &store.ProtoBlob{
		NamespaceID:    "acme",
		SHA256:         "abc123",
		OriginalSource: []byte("syntax = \"proto3\";"),
		SizeBytes:      18,
	}

	// Put blob.
	require.NoError(t, s.PutBlob(ctx, blob))

	// Get blob.
	got, err := s.GetBlob(ctx, "acme", "abc123")
	require.NoError(t, err)
	assert.Equal(t, blob.OriginalSource, got.OriginalSource)
	assert.Equal(t, 18, got.SizeBytes)

	// Put same blob again (idempotent).
	require.NoError(t, s.PutBlob(ctx, blob))
}

func TestVersionPutAndGet(t *testing.T) {
	s, ctx := setupStore(t)
	require.NoError(t, s.CreateNamespace(ctx, &store.Namespace{ID: "acme"}))
	require.NoError(t, s.CreateSchema(ctx, &store.Schema{NamespaceID: "acme", SchemaID: "billing"}))
	require.NoError(t, s.PutBlob(ctx, &store.ProtoBlob{
		NamespaceID: "acme", SHA256: "hash1",
		OriginalSource: []byte("content"), SizeBytes: 7,
	}))

	// Put version with files and deps.
	ver := &store.SchemaVersion{
		NamespaceID:     "acme",
		SchemaID:        "billing",
		Version:         1,
		Compiled:        []byte("compiled-fds"),
		CompilerVersion: "test@v0.0.1",
		CreatedBy:       "test-user",
		Metadata:        map[string]string{"ci": "true"},
	}
	files := []store.VersionFile{
		{NamespaceID: "acme", SchemaID: "billing", Version: 1, Filename: "billing/config.proto", BlobSHA256: "hash1"},
	}
	deps := []store.VersionDep{
		{NamespaceID: "acme", SchemaID: "billing", Version: 1, DepSchemaID: "common", DepFilename: "common/types.proto", DepVersion: 3},
	}
	require.NoError(t, s.PutVersion(ctx, ver, files, deps))

	// Get version.
	got, err := s.GetVersion(ctx, "acme", "billing", 1)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), got.Version)
	assert.Equal(t, []byte("compiled-fds"), got.Compiled)
	assert.Equal(t, "test@v0.0.1", got.CompilerVersion)
	assert.Equal(t, "test-user", got.CreatedBy)
	assert.Equal(t, "true", got.Metadata["ci"])

	// Get version files.
	gotFiles, err := s.GetVersionFiles(ctx, "acme", "billing", 1)
	require.NoError(t, err)
	require.Len(t, gotFiles, 1)
	assert.Equal(t, "billing/config.proto", gotFiles[0].Filename)
	assert.Equal(t, "hash1", gotFiles[0].BlobSHA256)

	// List versions.
	versions, err := s.ListVersions(ctx, "acme", "billing")
	require.NoError(t, err)
	assert.Equal(t, []uint64{1}, versions)
}

func TestStagingAndPromotion(t *testing.T) {
	s, ctx := setupStore(t)
	require.NoError(t, s.CreateNamespace(ctx, &store.Namespace{ID: "acme"}))
	require.NoError(t, s.CreateSchema(ctx, &store.Schema{NamespaceID: "acme", SchemaID: "billing"}))
	require.NoError(t, s.CreateSchema(ctx, &store.Schema{NamespaceID: "acme", SchemaID: "common"}))
	require.NoError(t, s.PutBlob(ctx, &store.ProtoBlob{
		NamespaceID: "acme", SHA256: "h1",
		OriginalSource: []byte("c1"), SizeBytes: 2,
	}))
	require.NoError(t, s.PutBlob(ctx, &store.ProtoBlob{
		NamespaceID: "acme", SHA256: "h2",
		OriginalSource: []byte("c2"), SizeBytes: 2,
	}))

	// Create versions.
	putVer := func(schemaID string, version uint64, hash string) {
		t.Helper()
		require.NoError(t, s.PutVersion(ctx, &store.SchemaVersion{
			NamespaceID: "acme", SchemaID: schemaID, Version: version,
			Compiled: []byte("compiled"), CompilerVersion: "test",
		}, []store.VersionFile{
			{NamespaceID: "acme", SchemaID: schemaID, Version: version, Filename: schemaID + "/f.proto", BlobSHA256: hash},
		}, nil))
	}
	putVer("billing", 1, "h1")
	putVer("common", 1, "h2")

	// Stage both.
	require.NoError(t, s.SetStaged(ctx, "acme", "billing", 1))
	require.NoError(t, s.SetStaged(ctx, "acme", "common", 1))

	// Verify staged.
	schema, err := s.GetSchema(ctx, "acme", "billing")
	require.NoError(t, err)
	require.NotNil(t, schema.StagedVersion)
	assert.Equal(t, uint64(1), *schema.StagedVersion)

	// Promote.
	promoted, err := s.Promote(ctx, "acme")
	require.NoError(t, err)
	assert.Len(t, promoted, 2)

	// Verify current is set, staged is cleared.
	schema, err = s.GetSchema(ctx, "acme", "billing")
	require.NoError(t, err)
	require.NotNil(t, schema.CurrentVersion)
	assert.Equal(t, uint64(1), *schema.CurrentVersion)
	assert.Nil(t, schema.StagedVersion)
}

func TestDiscardStaging(t *testing.T) {
	s, ctx := setupStore(t)
	require.NoError(t, s.CreateNamespace(ctx, &store.Namespace{ID: "acme"}))
	require.NoError(t, s.CreateSchema(ctx, &store.Schema{NamespaceID: "acme", SchemaID: "billing"}))
	require.NoError(t, s.PutBlob(ctx, &store.ProtoBlob{
		NamespaceID: "acme", SHA256: "h1",
		OriginalSource: []byte("c"), SizeBytes: 1,
	}))
	require.NoError(t, s.PutVersion(ctx, &store.SchemaVersion{
		NamespaceID: "acme", SchemaID: "billing", Version: 1,
		Compiled: []byte("c"), CompilerVersion: "test",
	}, []store.VersionFile{
		{NamespaceID: "acme", SchemaID: "billing", Version: 1, Filename: "f.proto", BlobSHA256: "h1"},
	}, nil))

	require.NoError(t, s.SetStaged(ctx, "acme", "billing", 1))

	// Discard.
	require.NoError(t, s.DiscardStaging(ctx, "acme"))

	schema, err := s.GetSchema(ctx, "acme", "billing")
	require.NoError(t, err)
	assert.Nil(t, schema.StagedVersion)
}

func TestLoadAllCurrent(t *testing.T) {
	s, ctx := setupStore(t)

	// Set up two namespaces with current versions.
	for _, nsID := range []string{"acme", "corp"} {
		require.NoError(t, s.CreateNamespace(ctx, &store.Namespace{ID: nsID}))
		require.NoError(t, s.CreateSchema(ctx, &store.Schema{NamespaceID: nsID, SchemaID: "config"}))
		require.NoError(t, s.PutBlob(ctx, &store.ProtoBlob{
			NamespaceID: nsID, SHA256: nsID + "-hash",
			OriginalSource: []byte(nsID), SizeBytes: len(nsID),
		}))
		require.NoError(t, s.PutVersion(ctx, &store.SchemaVersion{
			NamespaceID: nsID, SchemaID: "config", Version: 1,
			Compiled: []byte("compiled-" + nsID), CompilerVersion: "test",
		}, []store.VersionFile{
			{NamespaceID: nsID, SchemaID: "config", Version: 1, Filename: "config.proto", BlobSHA256: nsID + "-hash"},
		}, nil))

		// Stage and promote to make it current.
		require.NoError(t, s.SetStaged(ctx, nsID, "config", 1))
	}
	_, err := s.Promote(ctx, "acme")
	require.NoError(t, err)
	_, err = s.Promote(ctx, "corp")
	require.NoError(t, err)

	// Load all.
	schemas, err := s.LoadAllCurrent(ctx)
	require.NoError(t, err)
	assert.Len(t, schemas, 2)

	// Each should have files populated.
	for _, cs := range schemas {
		assert.NotEmpty(t, cs.Files, "files should be populated for %s/%s", cs.NamespaceID, cs.SchemaID)
		assert.NotEmpty(t, cs.Compiled)
	}
}

func TestLoadNamespaceProposed(t *testing.T) {
	s, ctx := setupStore(t)
	require.NoError(t, s.CreateNamespace(ctx, &store.Namespace{ID: "acme"}))
	require.NoError(t, s.CreateSchema(ctx, &store.Schema{NamespaceID: "acme", SchemaID: "billing"}))
	require.NoError(t, s.CreateSchema(ctx, &store.Schema{NamespaceID: "acme", SchemaID: "common"}))

	for _, h := range []string{"h1", "h2", "h3"} {
		require.NoError(t, s.PutBlob(ctx, &store.ProtoBlob{
			NamespaceID: "acme", SHA256: h,
			OriginalSource: []byte(h), SizeBytes: 2,
		}))
	}

	// billing v1 (will be current), common v1 (will be current), common v2 (will be staged).
	putVer := func(schema string, ver uint64, hash string) {
		require.NoError(t, s.PutVersion(ctx, &store.SchemaVersion{
			NamespaceID: "acme", SchemaID: schema, Version: ver,
			Compiled: []byte("compiled"), CompilerVersion: "test",
		}, []store.VersionFile{
			{NamespaceID: "acme", SchemaID: schema, Version: ver, Filename: schema + ".proto", BlobSHA256: hash},
		}, nil))
	}
	putVer("billing", 1, "h1")
	putVer("common", 1, "h2")
	putVer("common", 2, "h3")

	// Promote billing v1 and common v1 as current.
	require.NoError(t, s.SetStaged(ctx, "acme", "billing", 1))
	require.NoError(t, s.SetStaged(ctx, "acme", "common", 1))
	_, err := s.Promote(ctx, "acme")
	require.NoError(t, err)

	// Stage common v2.
	require.NoError(t, s.SetStaged(ctx, "acme", "common", 2))

	// Proposed = billing v1 (current) + common v2 (staged).
	proposed, err := s.LoadNamespaceProposed(ctx, "acme")
	require.NoError(t, err)
	assert.Len(t, proposed, 2)

	bySchema := make(map[string]*store.CurrentSchema)
	for _, cs := range proposed {
		bySchema[cs.SchemaID] = cs
	}
	assert.Equal(t, uint64(1), bySchema["billing"].Version, "billing should be current v1")
	assert.Equal(t, uint64(2), bySchema["common"].Version, "common should be staged v2")
}

func TestClearStagedVersion(t *testing.T) {
	s, ctx := setupStore(t)
	require.NoError(t, s.CreateNamespace(ctx, &store.Namespace{ID: "acme"}))
	require.NoError(t, s.CreateSchema(ctx, &store.Schema{NamespaceID: "acme", SchemaID: "billing"}))
	require.NoError(t, s.PutBlob(ctx, &store.ProtoBlob{
		NamespaceID: "acme", SHA256: "h1",
		OriginalSource: []byte("c"), SizeBytes: 1,
	}))
	require.NoError(t, s.PutVersion(ctx, &store.SchemaVersion{
		NamespaceID: "acme", SchemaID: "billing", Version: 1,
		Compiled: []byte("c"), CompilerVersion: "test",
	}, []store.VersionFile{
		{NamespaceID: "acme", SchemaID: "billing", Version: 1, Filename: "f.proto", BlobSHA256: "h1"},
	}, nil))

	require.NoError(t, s.SetStaged(ctx, "acme", "billing", 1))

	// Clear staged via version=0.
	require.NoError(t, s.SetStaged(ctx, "acme", "billing", 0))

	schema, err := s.GetSchema(ctx, "acme", "billing")
	require.NoError(t, err)
	assert.Nil(t, schema.StagedVersion)
}
