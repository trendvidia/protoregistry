// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package protoregistry_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	protoregistry "github.com/trendvidia/protoregistry"
	"github.com/trendvidia/protoregistry/store/postgres"
	"github.com/trendvidia/protoregistry/store/postgres/pgtest"
)

func setupRegistry(t *testing.T) (*protoregistry.Registry, context.Context) {
	t.Helper()
	res := pgtest.Setup(t)
	s := postgres.New(res.Pool)
	return protoregistry.New(s), context.Background()
}

var simpleProto = map[string][]byte{
	"billing/config.proto": []byte(`syntax = "proto3";
package billing;
message Config {
  string name = 1;
  int32 timeout_ms = 2;
}
`),
}

var simpleProtoV2 = map[string][]byte{
	"billing/config.proto": []byte(`syntax = "proto3";
package billing;
message Config {
  string name = 1;
  int32 timeout_ms = 2;
  string description = 3;
}
`),
}

var breakingProto = map[string][]byte{
	"billing/config.proto": []byte(`syntax = "proto3";
package billing;
message Config {
  string name = 1;
  string timeout_ms = 2;
}
`),
}

func TestPublishAndPromote(t *testing.T) {
	reg, ctx := setupRegistry(t)

	// Publish v1.
	result, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme",
		SchemaID:    "billing",
		Sources:     simpleProto,
		CreatedBy:   "test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), result.Version)
	assert.False(t, result.NoChange)
	assert.NotNil(t, result.Snapshot)

	// Not yet visible as current.
	assert.Nil(t, reg.Current("acme", "billing"))
	assert.NotNil(t, reg.Staged("acme", "billing"))

	// Promote.
	promResult, err := reg.Promote(ctx, "acme")
	require.NoError(t, err)
	assert.Len(t, promResult.Promoted, 1)
	assert.Equal(t, "billing", promResult.Promoted[0].SchemaID)

	// Now visible as current.
	snap := reg.Current("acme", "billing")
	require.NotNil(t, snap)
	assert.Equal(t, uint64(1), snap.Version())

	// Can create dynamic messages from the current snapshot.
	msg, err := snap.NewMessage("billing.Config")
	require.NoError(t, err)
	assert.NotNil(t, msg)
}

func TestPublishNoChange(t *testing.T) {
	reg, ctx := setupRegistry(t)

	// Publish and promote v1.
	_, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme", SchemaID: "billing",
		Sources: simpleProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "acme")
	require.NoError(t, err)

	// Publish same content again → no change.
	result, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme", SchemaID: "billing",
		Sources: simpleProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	assert.True(t, result.NoChange)
}

func TestPublishMultipleVersions(t *testing.T) {
	reg, ctx := setupRegistry(t)

	// v1.
	r1, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme", SchemaID: "billing",
		Sources: simpleProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), r1.Version)

	_, err = reg.Promote(ctx, "acme")
	require.NoError(t, err)

	// v2 with a new field.
	r2, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme", SchemaID: "billing",
		Sources: simpleProtoV2, CreatedBy: "test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), r2.Version)

	_, err = reg.Promote(ctx, "acme")
	require.NoError(t, err)

	snap := reg.Current("acme", "billing")
	require.NotNil(t, snap)
	assert.Equal(t, uint64(2), snap.Version())

	// New field should be visible.
	md, err := snap.FindMessageByName("billing.Config")
	require.NoError(t, err)
	assert.Equal(t, 3, md.Fields().Len())
}

func TestPromoteBlockedByIncompatibleChange(t *testing.T) {
	reg, ctx := setupRegistry(t)

	// Publish and promote v1.
	_, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme", SchemaID: "billing",
		Sources: simpleProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "acme")
	require.NoError(t, err)

	// Stage a breaking change (field type changed).
	_, err = reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme", SchemaID: "billing",
		Sources: breakingProto, CreatedBy: "test",
	})
	require.NoError(t, err)

	// Promote should fail.
	_, err = reg.Promote(ctx, "acme")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compatibility check failed")

	// Current should still be v1.
	snap := reg.Current("acme", "billing")
	require.NotNil(t, snap)
	assert.Equal(t, uint64(1), snap.Version())
}

func TestDiscardStaging(t *testing.T) {
	reg, ctx := setupRegistry(t)

	_, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme", SchemaID: "billing",
		Sources: simpleProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	assert.NotNil(t, reg.Staged("acme", "billing"))

	require.NoError(t, reg.DiscardStaging(ctx, "acme"))
	assert.Nil(t, reg.Staged("acme", "billing"))
}

func TestRollback(t *testing.T) {
	reg, ctx := setupRegistry(t)

	// Publish and promote v1.
	_, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme", SchemaID: "billing",
		Sources: simpleProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "acme")
	require.NoError(t, err)

	// Publish and promote v2.
	_, err = reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme", SchemaID: "billing",
		Sources: simpleProtoV2, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "acme")
	require.NoError(t, err)

	assert.Equal(t, uint64(2), reg.Current("acme", "billing").Version())

	// Rollback to v1 (stages v1). v2 added fields, so the compat check
	// would (correctly) reject this as breaking — pass Force to express
	// the test's intent of exercising the rollback path itself.
	require.NoError(t, reg.Rollback(ctx, "acme", "billing", 1,
		protoregistry.RollbackOptions{Force: true}))
	assert.NotNil(t, reg.Staged("acme", "billing"))
	assert.Equal(t, uint64(1), reg.Staged("acme", "billing").Version())

	// Promote the rollback.
	_, err = reg.Promote(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), reg.Current("acme", "billing").Version())
}

func TestRestore(t *testing.T) {
	res := pgtest.Setup(t)
	s := postgres.New(res.Pool)
	ctx := context.Background()

	// Set up state with first registry instance.
	reg1 := protoregistry.New(s)
	_, err := reg1.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme", SchemaID: "billing",
		Sources: simpleProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg1.Promote(ctx, "acme")
	require.NoError(t, err)

	// Create a new registry (simulating restart) and restore.
	reg2 := protoregistry.New(s)
	assert.Nil(t, reg2.Current("acme", "billing"), "new registry has no state")

	require.NoError(t, reg2.Restore(ctx))

	snap := reg2.Current("acme", "billing")
	require.NotNil(t, snap, "state restored after Restore()")
	assert.Equal(t, uint64(1), snap.Version())

	// Verify the restored snapshot is functional.
	msg, err := snap.NewMessage("billing.Config")
	require.NoError(t, err)
	assert.NotNil(t, msg)
}

func TestMultiSchemaCoordinatedPromote(t *testing.T) {
	reg, ctx := setupRegistry(t)

	commonProto := map[string][]byte{
		"common/types.proto": []byte(`syntax = "proto3";
package common;
message ID {
  string value = 1;
}
`),
	}

	billingWithImport := map[string][]byte{
		"billing/config.proto": []byte(`syntax = "proto3";
package billing;
import "common/types.proto";
message Config {
  string name = 1;
  common.ID id = 2;
}
`),
	}

	// Publish common first, promote.
	_, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme", SchemaID: "common",
		Sources: commonProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "acme")
	require.NoError(t, err)

	// Publish billing that imports common.
	result, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme", SchemaID: "billing",
		Sources: billingWithImport, CreatedBy: "test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), result.Version)

	_, err = reg.Promote(ctx, "acme")
	require.NoError(t, err)

	snap := reg.Current("acme", "billing")
	require.NotNil(t, snap)
	md, err := snap.FindMessageByName("billing.Config")
	require.NoError(t, err)
	assert.Equal(t, 2, md.Fields().Len())
}

func TestNamespaceIsolation(t *testing.T) {
	reg, ctx := setupRegistry(t)

	// Publish to two different namespaces.
	_, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme", SchemaID: "billing",
		Sources: simpleProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "acme")
	require.NoError(t, err)

	_, err = reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "corp", SchemaID: "billing",
		Sources: simpleProtoV2, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "corp")
	require.NoError(t, err)

	// Each namespace has its own version.
	acmeSnap := reg.Current("acme", "billing")
	corpSnap := reg.Current("corp", "billing")
	require.NotNil(t, acmeSnap)
	require.NotNil(t, corpSnap)

	acmeMd, _ := acmeSnap.FindMessageByName("billing.Config")
	corpMd, _ := corpSnap.FindMessageByName("billing.Config")
	assert.Equal(t, 2, acmeMd.Fields().Len(), "acme has 2 fields")
	assert.Equal(t, 3, corpMd.Fields().Len(), "corp has 3 fields")
}
