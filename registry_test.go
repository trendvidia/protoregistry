// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package protoregistry_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	protoregistry "github.com/trendvidia/protoregistry"
	"github.com/trendvidia/protoregistry/authz"
	"github.com/trendvidia/protoregistry/compiler"
	"github.com/trendvidia/protoregistry/store"
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

// ---------------------------------------------------------------------------
// Phase 2b: namespace hierarchy — chain-aware Publish + D2 + Restore.
// See docs/design/namespace-hierarchy.md.
// ---------------------------------------------------------------------------

// sharedCommonsProto defines a parent-tier file that a child namespace
// imports across the hierarchy.
var sharedCommonsProto = map[string][]byte{
	"shared/money.proto": []byte(`syntax = "proto3";
package shared;
message Money {
  string currency = 1;
  int64 amount = 2;
}
`),
}

func TestPublishWithParentChain(t *testing.T) {
	reg, ctx := setupRegistry(t)

	// Publish + promote the parent's shared schema.
	_, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme-shared", SchemaID: "commons",
		Sources: sharedCommonsProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "acme-shared")
	require.NoError(t, err)

	// Wire the child's in-memory parent pointer. In production this is set
	// by phase 3's SetNamespaceParent RPC; for now we set it directly on
	// the in-memory Namespace (the store column is exercised by
	// TestRestoreRebuildsParentChain below).
	child := reg.Namespaces().GetOrCreate("acme-billing")
	parent := reg.Namespaces().Get("acme-shared")
	require.NotNil(t, parent)
	child.SetParent(parent)

	// Publish a child schema that imports the parent's file via the chain.
	childProto := map[string][]byte{
		"billing/invoice.proto": []byte(`syntax = "proto3";
package billing;
import "shared/money.proto";
message Invoice {
  string id = 1;
  shared.Money total = 2;
}
`),
	}
	result, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme-billing", SchemaID: "invoice",
		Sources: childProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), result.Version)

	// The compiled snapshot should expose the child's message and resolve
	// the parent's Money type via its field reference.
	require.NotNil(t, result.Snapshot)
	invoice, err := result.Snapshot.FindMessageByName("billing.Invoice")
	require.NoError(t, err)
	require.Equal(t, 2, invoice.Fields().Len())
	money := invoice.Fields().Get(1).Message()
	require.NotNil(t, money)
	assert.Equal(t, "shared.Money", string(money.FullName()),
		"chain resolution should bind the field to the parent's Money type")
}

func TestPublishRejectedOnFQNConflict(t *testing.T) {
	reg, ctx := setupRegistry(t)

	// Parent defines shared.Money.
	_, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme-shared", SchemaID: "commons",
		Sources: sharedCommonsProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "acme-shared")
	require.NoError(t, err)

	// Child is linked to parent.
	child := reg.Namespaces().GetOrCreate("acme-billing")
	parent := reg.Namespaces().Get("acme-shared")
	require.NotNil(t, parent)
	child.SetParent(parent)

	// Child redefines shared.Money in its own file. D2 must reject.
	shadowProto := map[string][]byte{
		"billing/money.proto": []byte(`syntax = "proto3";
package shared;
message Money {
  string fake = 1;
}
`),
	}
	_, err = reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme-billing", SchemaID: "money",
		Sources: shadowProto, CreatedBy: "test",
	})
	require.Error(t, err, "D2 must reject shadowed FQNs across the chain")

	// The error should be an FQNConflictError naming the ancestor and FQN.
	var fqnErr *compiler.FQNConflictError
	require.ErrorAs(t, err, &fqnErr)
	assert.Equal(t, "acme-shared", fqnErr.AncestorNamespaceID)
	require.NotEmpty(t, fqnErr.Conflicts)
	assert.Equal(t, "shared.Money", fqnErr.Conflicts[0].FQN)
}

func TestRestoreRebuildsParentChain(t *testing.T) {
	res := pgtest.Setup(t)
	s := postgres.New(res.Pool)
	ctx := context.Background()

	// Set up with a first registry instance: publish parent, promote.
	reg1 := protoregistry.New(s)
	_, err := reg1.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme-shared", SchemaID: "commons",
		Sources: sharedCommonsProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg1.Promote(ctx, "acme-shared")
	require.NoError(t, err)

	// Create the child namespace at store level (it has no schema yet,
	// but the namespace row must exist to take a parent FK reference).
	require.NoError(t, s.CreateNamespace(ctx, &store.Namespace{ID: "acme-billing"}))

	// Set parent at the DB level. In production this is the phase 3 RPC;
	// here we call the store method directly to exercise the column.
	parentID := "acme-shared"
	require.NoError(t, s.SetNamespaceParent(ctx, "acme-billing", &parentID, "test"))

	// New registry instance simulating a restart.
	reg2 := protoregistry.New(s)
	require.NoError(t, reg2.Restore(ctx))

	// In-memory parent pointer should be rebuilt from the DB column.
	child := reg2.Namespaces().Get("acme-billing")
	require.NotNil(t, child, "child namespace exists in memory after Restore")
	parent := child.Parent()
	require.NotNil(t, parent, "parent pointer rebuilt from DB")
	assert.Equal(t, "acme-shared", parent.ID())
}

// ---------------------------------------------------------------------------
// Phase 3: SetNamespaceParent RPC, authz, audit-log writes (D9).
// ---------------------------------------------------------------------------

func TestSetNamespaceParent_PersistsAndAudits(t *testing.T) {
	res := pgtest.Setup(t)
	s := postgres.New(res.Pool)
	ctx := context.Background()
	reg := protoregistry.New(s)

	// Set up parent (via Publish, which implicitly creates the namespace).
	_, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "acme-shared", SchemaID: "commons",
		Sources: sharedCommonsProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "acme-shared")
	require.NoError(t, err)

	// Explicitly create the child namespace via the new Registry API.
	require.NoError(t, reg.CreateNamespace(ctx, &protoregistry.CreateNamespaceRequest{
		NamespaceID: "acme-billing",
		ActorID:     "bootstrap",
	}))

	// Re-parent.
	parentID := "acme-shared"
	require.NoError(t, reg.SetNamespaceParent(ctx, &protoregistry.SetNamespaceParentRequest{
		NamespaceID: "acme-billing",
		ParentID:    &parentID,
		ActorID:     "alice",
	}))

	// In-memory parent pointer updated.
	child := reg.Namespaces().Get("acme-billing")
	require.NotNil(t, child)
	require.NotNil(t, child.Parent())
	assert.Equal(t, "acme-shared", child.Parent().ID())

	// Audit log row written transactionally (D9).
	events, err := s.ListNamespaceParentEvents(ctx, "acme-billing", 10)
	require.NoError(t, err)
	require.NotEmpty(t, events, "re-parenting must produce an audit row")
	ev := events[0]
	assert.Equal(t, "acme-billing", ev.NamespaceID)
	assert.Equal(t, "alice", ev.ActorID)
	require.NotNil(t, ev.NewParentID)
	assert.Equal(t, "acme-shared", *ev.NewParentID)
	assert.Nil(t, ev.PreviousParentID, "first parent set → previous is NULL")
}

func TestSetNamespaceParent_CyclePrevented(t *testing.T) {
	res := pgtest.Setup(t)
	s := postgres.New(res.Pool)
	ctx := context.Background()
	reg := protoregistry.New(s)

	// Two namespaces.
	require.NoError(t, reg.CreateNamespace(ctx, &protoregistry.CreateNamespaceRequest{
		NamespaceID: "a", ActorID: "test",
	}))
	require.NoError(t, reg.CreateNamespace(ctx, &protoregistry.CreateNamespaceRequest{
		NamespaceID: "b", ActorID: "test",
	}))

	// a → b.
	bID := "b"
	require.NoError(t, reg.SetNamespaceParent(ctx, &protoregistry.SetNamespaceParentRequest{
		NamespaceID: "a", ParentID: &bID, ActorID: "test",
	}))

	// b → a would form a cycle.
	aID := "a"
	err := reg.SetNamespaceParent(ctx, &protoregistry.SetNamespaceParentRequest{
		NamespaceID: "b", ParentID: &aID, ActorID: "test",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrParentCycle)
}

func TestSetNamespaceParent_SelfReferenceRejected(t *testing.T) {
	res := pgtest.Setup(t)
	s := postgres.New(res.Pool)
	ctx := context.Background()
	reg := protoregistry.New(s)

	require.NoError(t, reg.CreateNamespace(ctx, &protoregistry.CreateNamespaceRequest{
		NamespaceID: "solo", ActorID: "test",
	}))
	selfID := "solo"
	err := reg.SetNamespaceParent(ctx, &protoregistry.SetNamespaceParentRequest{
		NamespaceID: "solo", ParentID: &selfID, ActorID: "test",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrParentCycle)
}

// denyAllAuth is an Authorizer that rejects every operation, used to
// verify that the registry actually consults the configured Authorizer.
type denyAllAuth struct{}

func (denyAllAuth) CanCreateNamespace(context.Context, string, *string) error {
	return authz.ErrPermissionDenied
}
func (denyAllAuth) CanSetNamespaceParent(context.Context, string, *string) error {
	return authz.ErrPermissionDenied
}
func (denyAllAuth) CanPublish(context.Context, string, string) error {
	return authz.ErrPermissionDenied
}
func (denyAllAuth) CanPromote(context.Context, string) error {
	return authz.ErrPermissionDenied
}
func (denyAllAuth) CanRebase(context.Context, string, string) error {
	return authz.ErrPermissionDenied
}

func TestAuthorizer_GatesPublish(t *testing.T) {
	res := pgtest.Setup(t)
	s := postgres.New(res.Pool)
	ctx := context.Background()
	reg := protoregistry.New(s, protoregistry.WithAuthorizer(denyAllAuth{}))

	_, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "denied", SchemaID: "x",
		Sources: simpleProto, CreatedBy: "test",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, authz.ErrPermissionDenied)
}

// ---------------------------------------------------------------------------
// Phase 4: Rebase + GetRebaseStatus.
// ---------------------------------------------------------------------------

// childImportingShared is a child schema source that imports
// shared/money.proto and uses shared.Money. Compatible with the
// sharedCommonsProto / sharedCommonsProtoV2 fixtures below.
var childImportingShared = map[string][]byte{
	"billing/invoice.proto": []byte(`syntax = "proto3";
package billing;
import "shared/money.proto";
message Invoice {
  string id = 1;
  shared.Money total = 2;
}
`),
}

// sharedCommonsProtoV2 is sharedCommonsProto with an extra field. The
// shape change is non-breaking (additive), so a child compiled against
// v1 still works after parent promotes to v2.
var sharedCommonsProtoV2 = map[string][]byte{
	"shared/money.proto": []byte(`syntax = "proto3";
package shared;
message Money {
  string currency = 1;
  int64 amount = 2;
  string memo = 3;
}
`),
}

func TestGetRebaseStatus_UpToDate(t *testing.T) {
	reg, ctx := setupRegistry(t)

	// Set up parent + child, both at version 1.
	_, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "shared-ns", SchemaID: "commons",
		Sources: sharedCommonsProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "shared-ns")
	require.NoError(t, err)

	require.NoError(t, reg.CreateNamespace(ctx, &protoregistry.CreateNamespaceRequest{
		NamespaceID: "billing-ns", ActorID: "test",
	}))
	parentID := "shared-ns"
	require.NoError(t, reg.SetNamespaceParent(ctx, &protoregistry.SetNamespaceParentRequest{
		NamespaceID: "billing-ns", ParentID: &parentID, ActorID: "test",
	}))

	_, err = reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "billing-ns", SchemaID: "invoice",
		Sources: childImportingShared, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "billing-ns")
	require.NoError(t, err)

	// Status with no parent change → not behind.
	status, err := reg.GetRebaseStatus(ctx, "billing-ns", "invoice")
	require.NoError(t, err)
	assert.False(t, status.RebaseAvailable, "no parent change → rebase not available")
	require.NotEmpty(t, status.PinStatuses, "child has at least one cross-namespace pin")
	for _, ps := range status.PinStatuses {
		assert.Equal(t, ps.PinnedVersion, ps.CurrentVersion,
			"pinned and current should match before parent promotion")
	}
}

func TestGetRebaseStatus_AvailableAfterParentPromotion(t *testing.T) {
	reg, ctx := setupRegistry(t)

	// Parent v1 published+promoted, child published+promoted against v1.
	_, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "shared-ns", SchemaID: "commons",
		Sources: sharedCommonsProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "shared-ns")
	require.NoError(t, err)

	require.NoError(t, reg.CreateNamespace(ctx, &protoregistry.CreateNamespaceRequest{
		NamespaceID: "billing-ns", ActorID: "test",
	}))
	parentID := "shared-ns"
	require.NoError(t, reg.SetNamespaceParent(ctx, &protoregistry.SetNamespaceParentRequest{
		NamespaceID: "billing-ns", ParentID: &parentID, ActorID: "test",
	}))

	_, err = reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "billing-ns", SchemaID: "invoice",
		Sources: childImportingShared, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "billing-ns")
	require.NoError(t, err)

	// Parent promotes v2 (additive change).
	_, err = reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "shared-ns", SchemaID: "commons",
		Sources: sharedCommonsProtoV2, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "shared-ns")
	require.NoError(t, err)

	// Child's pinned dep is still v1; parent's current is v2.
	status, err := reg.GetRebaseStatus(ctx, "billing-ns", "invoice")
	require.NoError(t, err)
	assert.True(t, status.RebaseAvailable, "parent moved forward → rebase available")
	require.NotEmpty(t, status.PinStatuses)
	for _, ps := range status.PinStatuses {
		assert.Equal(t, uint64(1), ps.PinnedVersion)
		assert.Equal(t, uint64(2), ps.CurrentVersion)
	}
}

func TestRebase_RepinsAgainstParentCurrent(t *testing.T) {
	reg, ctx := setupRegistry(t)

	// Set up parent v1, child against parent v1.
	_, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "shared-ns", SchemaID: "commons",
		Sources: sharedCommonsProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "shared-ns")
	require.NoError(t, err)

	require.NoError(t, reg.CreateNamespace(ctx, &protoregistry.CreateNamespaceRequest{
		NamespaceID: "billing-ns", ActorID: "test",
	}))
	parentID := "shared-ns"
	require.NoError(t, reg.SetNamespaceParent(ctx, &protoregistry.SetNamespaceParentRequest{
		NamespaceID: "billing-ns", ParentID: &parentID, ActorID: "test",
	}))

	pubResult, err := reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "billing-ns", SchemaID: "invoice",
		Sources: childImportingShared, CreatedBy: "test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), pubResult.Version)
	_, err = reg.Promote(ctx, "billing-ns")
	require.NoError(t, err)

	// Parent v2.
	_, err = reg.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "shared-ns", SchemaID: "commons",
		Sources: sharedCommonsProtoV2, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = reg.Promote(ctx, "shared-ns")
	require.NoError(t, err)

	// Rebase child. Produces a new child version with refreshed pins.
	rebaseResult, err := reg.Rebase(ctx, &protoregistry.RebaseRequest{
		NamespaceID: "billing-ns", SchemaID: "invoice", ActorID: "alice",
	})
	require.NoError(t, err)
	assert.False(t, rebaseResult.NoChange)
	assert.Equal(t, uint64(2), rebaseResult.Version, "rebase produces the next child version")

	// Promote and inspect the new pins.
	_, err = reg.Promote(ctx, "billing-ns")
	require.NoError(t, err)

	status, err := reg.GetRebaseStatus(ctx, "billing-ns", "invoice")
	require.NoError(t, err)
	assert.False(t, status.RebaseAvailable, "after rebase, child is up to date")
	for _, ps := range status.PinStatuses {
		assert.Equal(t, ps.PinnedVersion, ps.CurrentVersion, "pins now match parent's current")
	}
}

func TestRebase_AuthzGated(t *testing.T) {
	res := pgtest.Setup(t)
	s := postgres.New(res.Pool)
	ctx := context.Background()

	// Bootstrap with AllowAll: parent + child published.
	bootstrap := protoregistry.New(s)
	_, err := bootstrap.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "shared-ns", SchemaID: "commons",
		Sources: sharedCommonsProto, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = bootstrap.Promote(ctx, "shared-ns")
	require.NoError(t, err)

	require.NoError(t, bootstrap.CreateNamespace(ctx, &protoregistry.CreateNamespaceRequest{
		NamespaceID: "billing-ns", ActorID: "test",
	}))
	parentID := "shared-ns"
	require.NoError(t, bootstrap.SetNamespaceParent(ctx, &protoregistry.SetNamespaceParentRequest{
		NamespaceID: "billing-ns", ParentID: &parentID, ActorID: "test",
	}))
	_, err = bootstrap.Publish(ctx, &protoregistry.PublishRequest{
		NamespaceID: "billing-ns", SchemaID: "invoice",
		Sources: childImportingShared, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = bootstrap.Promote(ctx, "billing-ns")
	require.NoError(t, err)

	// Now switch to deny-all and attempt rebase.
	denied := protoregistry.New(s, protoregistry.WithAuthorizer(denyAllAuth{}))
	require.NoError(t, denied.Restore(ctx))
	_, err = denied.Rebase(ctx, &protoregistry.RebaseRequest{
		NamespaceID: "billing-ns", SchemaID: "invoice", ActorID: "test",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, authz.ErrPermissionDenied)
}

func TestAuthorizer_GatesSetNamespaceParent(t *testing.T) {
	res := pgtest.Setup(t)
	s := postgres.New(res.Pool)
	ctx := context.Background()

	// Bootstrap with AllowAll so we can create namespaces, then re-wrap
	// the store in a registry with the deny-all authorizer.
	bootstrap := protoregistry.New(s)
	require.NoError(t, bootstrap.CreateNamespace(ctx, &protoregistry.CreateNamespaceRequest{
		NamespaceID: "a", ActorID: "test",
	}))
	require.NoError(t, bootstrap.CreateNamespace(ctx, &protoregistry.CreateNamespaceRequest{
		NamespaceID: "b", ActorID: "test",
	}))

	denied := protoregistry.New(s, protoregistry.WithAuthorizer(denyAllAuth{}))
	bID := "b"
	err := denied.SetNamespaceParent(ctx, &protoregistry.SetNamespaceParentRequest{
		NamespaceID: "a", ParentID: &bID, ActorID: "test",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, authz.ErrPermissionDenied)
}
