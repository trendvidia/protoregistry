// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package resolve

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/trendvidia/protoregistry/namespace"
	"github.com/trendvidia/protoregistry/snapshot"
)

// Tests for phase 2c: namespace-chain-aware resolution + origin tracking
// on resolve.Resolver. See docs/design/namespace-hierarchy.md.
//
// All tests construct snapshots and namespaces in process — no DB needed.

// buildSnapshotWith creates a snapshot containing a single proto file
// that defines one message at the given fully-qualified location.
// The file path becomes "<schema>.proto"; the message lives at <pkg>.<msg>.
func buildSnapshotWith(t *testing.T, version uint64, file, pkg, msg string) *snapshot.Snapshot {
	t.Helper()
	fds := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{
			{
				Name:    proto.String(file),
				Package: proto.String(pkg),
				Syntax:  proto.String("proto3"),
				MessageType: []*descriptorpb.DescriptorProto{
					{
						Name: proto.String(msg),
						Field: []*descriptorpb.FieldDescriptorProto{
							{
								Name:     proto.String("id"),
								Number:   proto.Int32(1),
								Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
								Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
								JsonName: proto.String("id"),
							},
						},
					},
				},
			},
		},
	}
	snap, err := snapshot.New(version, fds)
	require.NoError(t, err)
	return snap
}

func TestResolver_LocalNamespaceHit(t *testing.T) {
	r := namespace.NewRegistry()
	ns := r.GetOrCreate("acme")
	ns.SetCurrent("billing", buildSnapshotWith(t, 1, "billing/invoice.proto", "billing", "Invoice"))

	resolver := NewResolver(ns)
	md, originID, err := resolver.FindMessageByNameWithOrigin("billing.Invoice")
	require.NoError(t, err)
	require.NotNil(t, md)
	assert.Equal(t, "acme", originID, "type defined locally → origin is the namespace itself")
}

func TestResolver_FallsThroughChainToParent(t *testing.T) {
	r := namespace.NewRegistry()
	parent := r.GetOrCreate("acme-shared")
	child := r.GetOrCreate("acme-billing")
	child.SetParent(parent)

	// Type lives only in parent.
	parent.SetCurrent("commons", buildSnapshotWith(t, 1, "shared/money.proto", "shared", "Money"))

	resolver := NewResolver(child)
	md, originID, err := resolver.FindMessageByNameWithOrigin("shared.Money")
	require.NoError(t, err, "chain walk should find type in parent")
	require.NotNil(t, md)
	assert.Equal(t, "acme-shared", originID, "origin should name the parent that contributed the type")
}

func TestResolver_FallsThroughMultipleAncestors(t *testing.T) {
	r := namespace.NewRegistry()
	grand := r.GetOrCreate("acme-org")
	parent := r.GetOrCreate("acme-team")
	child := r.GetOrCreate("acme-svc")
	parent.SetParent(grand)
	child.SetParent(parent)

	// Type lives only at the top of the chain.
	grand.SetCurrent("org-types", buildSnapshotWith(t, 1, "org/types.proto", "org", "OrgID"))

	resolver := NewResolver(child)
	md, originID, err := resolver.FindMessageByNameWithOrigin("org.OrgID")
	require.NoError(t, err, "chain walk should reach the grandparent")
	require.NotNil(t, md)
	assert.Equal(t, "acme-org", originID)
}

func TestResolver_NearestTierWinsOnCollision(t *testing.T) {
	// D2 prevents this at publish time, but the resolver should still
	// behave deterministically if it ever encounters in-memory state with
	// the same FQN at two tiers (e.g. a bug elsewhere or a test scenario).
	r := namespace.NewRegistry()
	parent := r.GetOrCreate("p")
	child := r.GetOrCreate("c")
	child.SetParent(parent)

	parent.SetCurrent("s", buildSnapshotWith(t, 1, "p.proto", "shared", "Money"))
	child.SetCurrent("s", buildSnapshotWith(t, 1, "c.proto", "shared", "Money"))

	resolver := NewResolver(child)
	_, originID, err := resolver.FindMessageByNameWithOrigin("shared.Money")
	require.NoError(t, err)
	assert.Equal(t, "c", originID, "nearest tier (child) wins")
}

func TestResolver_NotFound(t *testing.T) {
	r := namespace.NewRegistry()
	ns := r.GetOrCreate("acme")
	ns.SetCurrent("billing", buildSnapshotWith(t, 1, "billing/invoice.proto", "billing", "Invoice"))

	resolver := NewResolver(ns)
	md, originID, err := resolver.FindMessageByNameWithOrigin("missing.Type")
	assert.Error(t, err, "missing type returns error")
	assert.Nil(t, md)
	assert.Empty(t, originID, "origin is empty on miss")
}

func TestResolver_FindFileByPathOriginWalksChain(t *testing.T) {
	r := namespace.NewRegistry()
	parent := r.GetOrCreate("p")
	child := r.GetOrCreate("c")
	child.SetParent(parent)

	parent.SetCurrent("s", buildSnapshotWith(t, 1, "shared/money.proto", "shared", "Money"))

	resolver := NewResolver(child)
	fd, originID, err := resolver.FindFileByPathWithOrigin("shared/money.proto")
	require.NoError(t, err)
	require.NotNil(t, fd)
	assert.Equal(t, "p", originID)
}

func TestResolver_FindDescriptorByNameOriginWalksChain(t *testing.T) {
	r := namespace.NewRegistry()
	parent := r.GetOrCreate("p")
	child := r.GetOrCreate("c")
	child.SetParent(parent)

	parent.SetCurrent("s", buildSnapshotWith(t, 1, "x.proto", "shared", "Thing"))

	resolver := NewResolver(child)
	d, originID, err := resolver.FindDescriptorByNameWithOrigin(protoreflect.FullName("shared.Thing"))
	require.NoError(t, err)
	require.NotNil(t, d)
	assert.Equal(t, "p", originID)
}

func TestResolver_NewMessageWalksChain(t *testing.T) {
	r := namespace.NewRegistry()
	parent := r.GetOrCreate("p")
	child := r.GetOrCreate("c")
	child.SetParent(parent)

	parent.SetCurrent("s", buildSnapshotWith(t, 1, "shared/money.proto", "shared", "Money"))

	resolver := NewResolver(child)
	msg, err := resolver.NewMessage("shared.Money")
	require.NoError(t, err, "NewMessage walks the chain via FindMessageByName")
	require.NotNil(t, msg)
}

func TestResolver_RootNamespaceBehavesAsBefore(t *testing.T) {
	// A namespace with no parent behaves exactly as the pre-2c Resolver:
	// lookups succeed for local types and fail for anything else.
	r := namespace.NewRegistry()
	ns := r.GetOrCreate("solo")
	ns.SetCurrent("s", buildSnapshotWith(t, 1, "x.proto", "x", "X"))

	resolver := NewResolver(ns)

	md, originID, err := resolver.FindMessageByNameWithOrigin("x.X")
	require.NoError(t, err)
	require.NotNil(t, md)
	assert.Equal(t, "solo", originID)

	_, originID, err = resolver.FindMessageByNameWithOrigin("not.Defined")
	require.Error(t, err)
	assert.Empty(t, originID)
}

// TestResolver_BackwardCompatNonOriginAPI verifies that the existing
// (non-origin) lookup methods still work after the refactor and now
// transparently benefit from chain walking.
func TestResolver_BackwardCompatNonOriginAPI(t *testing.T) {
	r := namespace.NewRegistry()
	parent := r.GetOrCreate("p")
	child := r.GetOrCreate("c")
	child.SetParent(parent)

	parent.SetCurrent("s", buildSnapshotWith(t, 1, "shared/money.proto", "shared", "Money"))

	resolver := NewResolver(child)

	// All four "without origin" methods should now succeed via chain walk.
	fd, err := resolver.FindFileByPath("shared/money.proto")
	require.NoError(t, err)
	require.NotNil(t, fd)

	d, err := resolver.FindDescriptorByName("shared.Money")
	require.NoError(t, err)
	require.NotNil(t, d)

	mt, err := resolver.FindMessageByName("shared.Money")
	require.NoError(t, err)
	require.NotNil(t, mt)

	msg, err := resolver.NewMessage("shared.Money")
	require.NoError(t, err)
	require.NotNil(t, msg)
}
