// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package client_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/trendvidia/protoregistry/client"
	"github.com/trendvidia/protoregistry/client/internal/clienttest"
)

const (
	billingV1 = `syntax = "proto3";
package billing;
message Config {
  string name = 1;
}
`
	billingV2 = `syntax = "proto3";
package billing;
message Config {
  string name = 1;
  int32 timeout_ms = 2;
}
`
)

// TestIntegration consolidates every scenario that needs a running
// server into one t.Run tree so we pay the Postgres-container startup
// cost (~5–10s) once.
func TestIntegration(t *testing.T) {
	srv := clienttest.Start(t)

	t.Run("FindDescriptorByName", func(t *testing.T) {
		const ns = "lookup"
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})

		r, err := client.New(t.Context(), srv.Conn, ns, client.WithRefreshInterval(0))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		desc, err := r.FindDescriptorByName("billing.Config")
		require.NoError(t, err)
		require.Equal(t, protoreflect.FullName("billing.Config"), desc.FullName())

		// The dynamic message should round-trip the schema.
		msg, err := r.NewMessage("billing.Config")
		require.NoError(t, err)
		require.NotNil(t, msg.Descriptor().Fields().ByName("name"))
	})

	t.Run("FindMessageByURL", func(t *testing.T) {
		const ns = "byurl"
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})

		r, err := client.New(t.Context(), srv.Conn, ns, client.WithRefreshInterval(0))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		mt, err := r.FindMessageByURL("type.googleapis.com/billing.Config")
		require.NoError(t, err)
		require.Equal(t, protoreflect.FullName("billing.Config"), mt.Descriptor().FullName())
	})

	t.Run("HotSwap", func(t *testing.T) {
		const ns = "hotswap"
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})

		r, err := client.New(t.Context(), srv.Conn, ns, client.WithRefreshInterval(0))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		// v1: only "name" exists.
		msg, err := r.NewMessage("billing.Config")
		require.NoError(t, err)
		require.NotNil(t, msg.Descriptor().Fields().ByName("name"))
		require.Nil(t, msg.Descriptor().Fields().ByName("timeout_ms"))

		// Promote v2 and force refresh.
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV2),
		})
		require.NoError(t, r.Refresh(t.Context()))

		// v2: timeout_ms is now visible.
		msg, err = r.NewMessage("billing.Config")
		require.NoError(t, err)
		require.NotNil(t, msg.Descriptor().Fields().ByName("timeout_ms"))
	})

	t.Run("IncrementalRefresh_AddAndReplace", func(t *testing.T) {
		// Exercises the diff path in Refresh: one schema is replaced
		// (version advance) while another is added (brand new) in the
		// same refresh cycle. The aggregate registries (nsFiles,
		// nsTypes) must reflect both diffs after a single refresh.
		const ns = "incremental"
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})

		r, err := client.New(t.Context(), srv.Conn, ns, client.WithRefreshInterval(0))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		// Initial state: only billing v1 exists.
		_, err = r.FindFileByPath("billing.proto")
		require.NoError(t, err)
		_, err = r.FindFileByPath("audit.proto")
		require.ErrorIs(t, err, protoregistry.NotFound)

		// In one server cycle: replace billing with v2 AND add a new
		// schema "audit". Both visible after a single Refresh.
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV2),
		})
		srv.PublishAndPromote(t, ns, "audit", map[string][]byte{
			"audit.proto": []byte(`syntax = "proto3"; package audit; message Event { string actor = 1; }`),
		})
		require.NoError(t, r.Refresh(t.Context()))

		// REPLACE: billing now has timeout_ms.
		billingMsg, err := r.NewMessage("billing.Config")
		require.NoError(t, err)
		require.NotNil(t, billingMsg.Descriptor().Fields().ByName("timeout_ms"),
			"billing.proto should be at v2 after refresh")

		// ADD: audit is now resolvable across every lookup path.
		auditDesc, err := r.FindDescriptorByName("audit.Event")
		require.NoError(t, err)
		require.Equal(t, protoreflect.FullName("audit.Event"), auditDesc.FullName())

		auditFile, err := r.FindFileByPath("audit.proto")
		require.NoError(t, err)
		require.Equal(t, "audit.proto", auditFile.Path())

		// SchemaResolver isolation still holds after the diff.
		_, err = r.Schema("audit").FindMessageByName("billing.Config")
		require.ErrorIs(t, err, protoregistry.NotFound)
	})

	t.Run("Pin_FreezesAtVersion", func(t *testing.T) {
		const ns = "pin"
		v1 := srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV2),
		})

		r, err := client.New(t.Context(), srv.Conn, ns, client.WithRefreshInterval(0))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		// Live resolver sees v2.
		msg, err := r.NewMessage("billing.Config")
		require.NoError(t, err)
		require.NotNil(t, msg.Descriptor().Fields().ByName("timeout_ms"))

		// Pinned to v1: timeout_ms is gone again.
		pinned, err := r.Pin(t.Context(), map[string]uint64{"billing": v1})
		require.NoError(t, err)
		t.Cleanup(func() { _ = pinned.Close() })

		pinnedMsg, err := pinned.NewMessage("billing.Config")
		require.NoError(t, err)
		require.NotNil(t, pinnedMsg.Descriptor().Fields().ByName("name"))
		require.Nil(t, pinnedMsg.Descriptor().Fields().ByName("timeout_ms"))
	})

	t.Run("WithSchemas_Filter", func(t *testing.T) {
		const ns = "filter"
		srv.PublishAndPromote(t, ns, "kept", map[string][]byte{
			"kept.proto": []byte(`syntax = "proto3"; package kept; message K { string a = 1; }`),
		})
		srv.PublishAndPromote(t, ns, "skipped", map[string][]byte{
			"skipped.proto": []byte(`syntax = "proto3"; package skipped; message S { string a = 1; }`),
		})

		r, err := client.New(t.Context(), srv.Conn, ns,
			client.WithRefreshInterval(0),
			client.WithSchemas("kept"),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		_, err = r.FindDescriptorByName("kept.K")
		require.NoError(t, err)

		_, err = r.FindDescriptorByName("skipped.S")
		require.ErrorIs(t, err, protoregistry.NotFound)
	})

	t.Run("WithFallback_ResolvesParentTypes", func(t *testing.T) {
		// A child Resolver with WithFallback configured must resolve
		// types that exist only in the parent registry — verifying the
		// fork's hierarchical fallback is wired through every lookup
		// tier (FindMessageByName, FindFileByPath,
		// FindDescriptorByName) and reachable via SchemaResolver too.
		const ns = "fallback"
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})

		// Build a parent registry by repurposing a separate Resolver
		// pointed at a different namespace ("commons") that exposes
		// types we want every child to inherit.
		srv.PublishAndPromote(t, "commons", "shared", map[string][]byte{
			"shared.proto": []byte(`syntax = "proto3"; package shared; message Trace { string id = 1; }`),
		})
		parent, err := client.New(t.Context(), srv.Conn, "commons", client.WithRefreshInterval(0))
		require.NoError(t, err)
		t.Cleanup(func() { _ = parent.Close() })

		// Child Resolver: tracks "fallback" namespace, falls back to
		// the "commons" parent for any miss.
		child, err := client.New(t.Context(), srv.Conn, ns,
			client.WithRefreshInterval(0),
			client.WithParent(parent),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = child.Close() })

		// Local lookup hits the child's own namespace.
		_, err = child.FindDescriptorByName("billing.Config")
		require.NoError(t, err)

		// Parent-only type resolves through the fallback chain at
		// every lookup tier on the child.
		desc, err := child.FindDescriptorByName("shared.Trace")
		require.NoError(t, err, "FindDescriptorByName must walk to parent")
		require.Equal(t, protoreflect.FullName("shared.Trace"), desc.FullName())

		mt, err := child.FindMessageByName("shared.Trace")
		require.NoError(t, err, "FindMessageByName must walk to parent")
		require.Equal(t, protoreflect.FullName("shared.Trace"), mt.Descriptor().FullName())

		fd, err := child.FindFileByPath("shared.proto")
		require.NoError(t, err, "FindFileByPath must walk to parent")
		require.Equal(t, "shared.proto", fd.Path())

		// Per-schema lookup also walks to the parent — schema views
		// are constructed with the same parent fallback.
		schemaDesc, err := child.Schema("billing").FindMessageByName("shared.Trace")
		require.NoError(t, err, "SchemaResolver must walk to parent")
		require.Equal(t, protoreflect.FullName("shared.Trace"), schemaDesc.Descriptor().FullName())

		// A name absent from both child and parent stays NotFound.
		_, err = child.FindDescriptorByName("nowhere.Missing")
		require.ErrorIs(t, err, protoregistry.NotFound)
	})

	t.Run("SchemaResolver", func(t *testing.T) {
		const ns = "schemares"
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})

		r, err := client.New(t.Context(), srv.Conn, ns, client.WithRefreshInterval(0))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		schema := r.Schema("billing")
		require.Equal(t, "billing", schema.SchemaID())

		msg, err := schema.NewMessage("billing.Config")
		require.NoError(t, err)
		require.NotNil(t, msg.Descriptor().Fields().ByName("name"))

		// Schema that doesn't exist returns NotFound from FindMessageByName.
		missing := r.Schema("nope")
		_, err = missing.FindMessageByName("billing.Config")
		require.ErrorIs(t, err, protoregistry.NotFound)
	})

	t.Run("WithServerChain_ResolvesAncestorTypes", func(t *testing.T) {
		// Server-authoritative hierarchy: child is set as a child of
		// parent via SetNamespaceParent; client.New with WithServerChain
		// then discovers the chain automatically and resolves parent's
		// types through the auto-wired ancestor Resolver.
		const (
			parentNS = "wsc-shared"
			childNS  = "wsc-billing"
		)
		srv.PublishAndPromote(t, parentNS, "commons", map[string][]byte{
			"shared/money.proto": []byte(`syntax = "proto3";
package shared;
message Money {
  string currency = 1;
  int64 amount = 2;
}
`),
		})
		srv.CreateNamespace(t, childNS)
		srv.SetNamespaceParent(t, childNS, parentNS)

		// Child publishes a schema importing the parent's file. The
		// publish itself walks the chain on the server side; the test
		// here is that the *client* can see the parent's types via
		// chain expansion at construction time.
		srv.PublishAndPromote(t, childNS, "invoice", map[string][]byte{
			"billing/invoice.proto": []byte(`syntax = "proto3";
package billing;
import "shared/money.proto";
message Invoice {
  string id = 1;
  shared.Money total = 2;
}
`),
		})

		r, err := client.New(t.Context(), srv.Conn, childNS,
			client.WithRefreshInterval(0),
			client.WithServerChain(),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		// Child's own type — local lookup.
		invoice, err := r.FindMessageByName("billing.Invoice")
		require.NoError(t, err)
		require.Equal(t, protoreflect.FullName("billing.Invoice"), invoice.Descriptor().FullName())

		// Parent's type — only reachable because WithServerChain wired
		// up the ancestor Resolver as parent.
		money, err := r.FindMessageByName("shared.Money")
		require.NoError(t, err)
		require.Equal(t, protoreflect.FullName("shared.Money"), money.Descriptor().FullName())
	})

	t.Run("WithServerChain_RootNamespaceWorksWithoutAncestors", func(t *testing.T) {
		// A namespace with no parent should produce a single-element
		// chain. WithServerChain on it must work the same as not using
		// the option — proves the chain expansion handles the trivial
		// case correctly.
		const ns = "wsc-root"
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})

		r, err := client.New(t.Context(), srv.Conn, ns,
			client.WithRefreshInterval(0),
			client.WithServerChain(),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		_, err = r.FindMessageByName("billing.Config")
		require.NoError(t, err)
	})

	t.Run("RangeMessages_EnumeratesAllVisibleTypes", func(t *testing.T) {
		// RangeMessages must yield every message type the Resolver can
		// resolve — both the bound namespace's own types and any
		// inherited via the parent/fallback chain. Useful for editor
		// integrations that populate completion lists of known FQNs.
		srv.PublishAndPromote(t, "rangemsg-shared", "commons", map[string][]byte{
			"shared.proto": []byte(`syntax = "proto3"; package shared; message Money { string currency = 1; }`),
		})
		parent, err := client.New(t.Context(), srv.Conn, "rangemsg-shared", client.WithRefreshInterval(0))
		require.NoError(t, err)
		t.Cleanup(func() { _ = parent.Close() })

		srv.PublishAndPromote(t, "rangemsg-billing", "invoice", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})
		child, err := client.New(t.Context(), srv.Conn, "rangemsg-billing",
			client.WithRefreshInterval(0),
			client.WithParent(parent),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = child.Close() })

		seen := map[protoreflect.FullName]bool{}
		child.RangeMessages(func(mt protoreflect.MessageType) bool {
			seen[mt.Descriptor().FullName()] = true
			return true
		})

		require.True(t, seen["billing.Config"], "child's own type must be enumerated")
		require.True(t, seen["shared.Money"], "parent type must surface through the fallback chain")
	})

	t.Run("RangeMessages_StopsOnFalse", func(t *testing.T) {
		// The contract for RangeXxx callbacks: returning false halts
		// iteration. The walker shouldn't invoke the callback again
		// after that.
		srv.PublishAndPromote(t, "rangemsg-halt", "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})
		r, err := client.New(t.Context(), srv.Conn, "rangemsg-halt", client.WithRefreshInterval(0))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		calls := 0
		r.RangeMessages(func(_ protoreflect.MessageType) bool {
			calls++
			return false
		})
		require.Equal(t, 1, calls, "RangeMessages must stop after the callback returns false")
	})

	t.Run("FindMessageByNameWithOrigin_OwnNamespace", func(t *testing.T) {
		// A type defined in the resolver's bound namespace resolves
		// with origin == namespace ID.
		const ns = "origin-own"
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})

		r, err := client.New(t.Context(), srv.Conn, ns, client.WithRefreshInterval(0))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		mt, origin, err := r.FindMessageByNameWithOrigin("billing.Config")
		require.NoError(t, err)
		require.Equal(t, protoreflect.FullName("billing.Config"), mt.Descriptor().FullName())
		require.Equal(t, ns, origin, "own-namespace types must report the resolver's own namespace")
	})

	t.Run("FindMessageByNameWithOrigin_ServerChain_ReportsAncestor", func(t *testing.T) {
		// Parent has shared.Money; child binds to its own namespace.
		// Use the existing testcontainers chain wiring: child's
		// namespace has a server-side parent pointer to shared-ns,
		// so WithServerChain pulls ancestor types and FindMessage*
		// WithOrigin returns the ancestor's namespace ID.
		const (
			parentNS = "origin-shared"
			childNS  = "origin-billing"
		)
		srv.PublishAndPromote(t, parentNS, "commons", map[string][]byte{
			"shared/money.proto": []byte(`syntax = "proto3"; package shared; message Money { string currency = 1; }`),
		})
		srv.CreateNamespace(t, childNS)
		srv.SetNamespaceParent(t, childNS, parentNS)
		srv.PublishAndPromote(t, childNS, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})

		r, err := client.New(t.Context(), srv.Conn, childNS,
			client.WithRefreshInterval(0),
			client.WithServerChain(),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		// Child's own type: origin is childNS.
		_, origin, err := r.FindMessageByNameWithOrigin("billing.Config")
		require.NoError(t, err)
		require.Equal(t, childNS, origin)

		// Parent's type: origin is parentNS.
		_, origin, err = r.FindMessageByNameWithOrigin("shared.Money")
		require.NoError(t, err)
		require.Equal(t, parentNS, origin, "ancestor types must report the contributing namespace")
	})

	t.Run("FindMessageByNameWithOrigin_AdHocParentHasEmptyOrigin", func(t *testing.T) {
		// WithParent supplies a parent registry but not its namespace
		// identity beyond what the Resolver itself remembers. When
		// the lookup hits the parent's tier, origin is "" — the
		// ad-hoc parent registries don't carry an ID. Documented in
		// the FindMessageByNameWithOrigin godoc.
		srv.PublishAndPromote(t, "origin-fbparent", "commons", map[string][]byte{
			"shared.proto": []byte(`syntax = "proto3"; package shared; message Trace { string id = 1; }`),
		})
		parent, err := client.New(t.Context(), srv.Conn, "origin-fbparent", client.WithRefreshInterval(0))
		require.NoError(t, err)
		t.Cleanup(func() { _ = parent.Close() })

		srv.PublishAndPromote(t, "origin-fbchild", "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})
		child, err := client.New(t.Context(), srv.Conn, "origin-fbchild",
			client.WithRefreshInterval(0),
			client.WithParent(parent),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = child.Close() })

		// Child's own type: origin populated.
		_, origin, err := child.FindMessageByNameWithOrigin("billing.Config")
		require.NoError(t, err)
		require.Equal(t, "origin-fbchild", origin)

		// Parent's type via WithParent: hits cfg.parentTypes, no namespace identity.
		_, origin, err = child.FindMessageByNameWithOrigin("shared.Trace")
		require.NoError(t, err)
		require.Equal(t, "", origin,
			"WithParent supplies registries only — namespace identity is the empty string")
	})

	t.Run("FindMessageByNameWithOrigin_NotFound", func(t *testing.T) {
		const ns = "origin-notfound"
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})
		r, err := client.New(t.Context(), srv.Conn, ns, client.WithRefreshInterval(0))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		mt, origin, err := r.FindMessageByNameWithOrigin("nowhere.Missing")
		require.ErrorIs(t, err, protoregistry.NotFound)
		require.Nil(t, mt)
		require.Equal(t, "", origin, "NotFound produces zero-valued origin")
	})

	t.Run("FindFileByPathWithOrigin_ServerChain", func(t *testing.T) {
		// Mirror of the message test for the file lookup path.
		// The file lives in the parent namespace; child uses
		// WithServerChain to expand the chain.
		const (
			parentNS = "originfp-shared"
			childNS  = "originfp-billing"
		)
		srv.PublishAndPromote(t, parentNS, "commons", map[string][]byte{
			"shared/money.proto": []byte(`syntax = "proto3"; package shared; message Money { string currency = 1; }`),
		})
		srv.CreateNamespace(t, childNS)
		srv.SetNamespaceParent(t, childNS, parentNS)
		srv.PublishAndPromote(t, childNS, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})

		r, err := client.New(t.Context(), srv.Conn, childNS,
			client.WithRefreshInterval(0),
			client.WithServerChain(),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		fd, origin, err := r.FindFileByPathWithOrigin("billing.proto")
		require.NoError(t, err)
		require.Equal(t, "billing.proto", fd.Path())
		require.Equal(t, childNS, origin)

		fd, origin, err = r.FindFileByPathWithOrigin("shared/money.proto")
		require.NoError(t, err)
		require.Equal(t, "shared/money.proto", fd.Path())
		require.Equal(t, parentNS, origin)
	})

	t.Run("GetSource_OwnNamespace", func(t *testing.T) {
		// GetSource returns the original .proto source bytes for a file
		// owned by the bound namespace. Verifies the schema-discovery
		// walk (filePath → schemaSnapshot) finds the right owner and
		// fetches its bytes via the gRPC GetSource RPC.
		const ns = "getsource-own"
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})

		r, err := client.New(t.Context(), srv.Conn, ns, client.WithRefreshInterval(0))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		src, err := r.GetSource(t.Context(), "billing.proto")
		require.NoError(t, err)
		require.Equal(t, billingV1, string(src), "GetSource must return the exact published source bytes")
	})

	t.Run("GetSource_ServerChain_FetchesFromAncestor", func(t *testing.T) {
		// A file lives only in the parent namespace; WithServerChain
		// wires an ancestor Resolver so GetSource walks to it.
		const (
			parentNS = "getsource-shared"
			childNS  = "getsource-billing"
		)
		const sharedSrc = `syntax = "proto3";
package shared;
message Money {
  string currency = 1;
  int64 amount = 2;
}
`
		srv.PublishAndPromote(t, parentNS, "commons", map[string][]byte{
			"shared/money.proto": []byte(sharedSrc),
		})
		srv.CreateNamespace(t, childNS)
		srv.SetNamespaceParent(t, childNS, parentNS)
		srv.PublishAndPromote(t, childNS, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})

		r, err := client.New(t.Context(), srv.Conn, childNS,
			client.WithRefreshInterval(0),
			client.WithServerChain(),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		// Child-owned file.
		src, err := r.GetSource(t.Context(), "billing.proto")
		require.NoError(t, err)
		require.Equal(t, billingV1, string(src))

		// Parent-owned file: only reachable through the ancestor walk.
		src, err = r.GetSource(t.Context(), "shared/money.proto")
		require.NoError(t, err, "GetSource must walk to ancestor namespaces for chain-inherited files")
		require.Equal(t, sharedSrc, string(src))
	})

	t.Run("GetSource_NotFound", func(t *testing.T) {
		// An unknown path returns NotFound (not a server error).
		const ns = "getsource-notfound"
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})
		r, err := client.New(t.Context(), srv.Conn, ns, client.WithRefreshInterval(0))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		src, err := r.GetSource(t.Context(), "does/not/exist.proto")
		require.ErrorIs(t, err, protoregistry.NotFound, "unknown paths return NotFound, not a server error")
		require.Nil(t, src)
	})

	t.Run("GetSource_AdHocParentReturnsNotFound", func(t *testing.T) {
		// WithParent supplies a pre-compiled registry but no registry
		// connection — files only reachable through that tier resolve
		// via FindFileByPath but produce NotFound from GetSource.
		// Documented in the GetSource godoc.
		const sharedSrc = `syntax = "proto3"; package shared; message Trace { string id = 1; }`
		srv.PublishAndPromote(t, "getsource-fbparent", "commons", map[string][]byte{
			"shared.proto": []byte(sharedSrc),
		})
		parent, err := client.New(t.Context(), srv.Conn, "getsource-fbparent", client.WithRefreshInterval(0))
		require.NoError(t, err)
		t.Cleanup(func() { _ = parent.Close() })

		srv.PublishAndPromote(t, "getsource-fbchild", "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})
		child, err := client.New(t.Context(), srv.Conn, "getsource-fbchild",
			client.WithRefreshInterval(0),
			client.WithParent(parent),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = child.Close() })

		// Child's own file: fetches fine.
		src, err := child.GetSource(t.Context(), "billing.proto")
		require.NoError(t, err)
		require.Equal(t, billingV1, string(src))

		// Parent's file: reachable via FindFileByPath but not GetSource.
		_, err = child.FindFileByPath("shared.proto")
		require.NoError(t, err, "ad-hoc parent's file IS resolvable as a descriptor")
		_, err = child.GetSource(t.Context(), "shared.proto")
		require.ErrorIs(t, err, protoregistry.NotFound,
			"ad-hoc parent (WithParent) doesn't expose a connection — GetSource returns NotFound")
	})

	t.Run("DiskCache_PersistsAndReloads", func(t *testing.T) {
		// Populate a cache via the live server, then verify the
		// manifest + descriptor files exist on disk. Reloading from
		// disk (the offline path) is covered in TestDiskCacheOffline.
		const ns = "cache-roundtrip"
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})

		cacheDir := t.TempDir()
		live, err := client.New(t.Context(), srv.Conn, ns,
			client.WithRefreshInterval(0),
			client.WithDiskCache(cacheDir),
		)
		require.NoError(t, err)
		require.False(t, live.IsStale(), "online Resolver must not report stale")
		t.Cleanup(func() { _ = live.Close() })

		manPath := filepath.Join(cacheDir, ns, "manifest.json")
		_, err = os.Stat(manPath)
		require.NoError(t, err, "manifest.json must be written after initial populate")

		schemaPath := filepath.Join(cacheDir, ns, "schemas", "billing@1.pb")
		_, err = os.Stat(schemaPath)
		require.NoError(t, err, "schema descriptor file must be written")
	})

	t.Run("DiskCache_RefreshOverwritesCache", func(t *testing.T) {
		// After a Refresh picks up a new schema version, the cache
		// reflects the new bytes via the version-in-filename scheme.
		const ns = "cache-refresh"
		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})

		cacheDir := t.TempDir()
		r, err := client.New(t.Context(), srv.Conn, ns,
			client.WithRefreshInterval(0),
			client.WithDiskCache(cacheDir),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		_, err = os.Stat(filepath.Join(cacheDir, ns, "schemas", "billing@1.pb"))
		require.NoError(t, err)

		srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
			"billing.proto": []byte(billingV2),
		})
		require.NoError(t, r.Refresh(t.Context()))

		_, err = os.Stat(filepath.Join(cacheDir, ns, "schemas", "billing@2.pb"))
		require.NoError(t, err, "post-refresh cache must include the new version file")
	})

	t.Run("DiskCache_ServerChainPersistsAncestors", func(t *testing.T) {
		// With WithServerChain, ancestor namespaces persist into
		// their own subdirectories under the same cacheDir, and the
		// top-level manifest records the chain so the offline loader
		// can rebuild it without server access.
		const (
			parentNS = "cache-chain-shared"
			childNS  = "cache-chain-billing"
		)
		srv.PublishAndPromote(t, parentNS, "commons", map[string][]byte{
			"shared/money.proto": []byte(`syntax = "proto3"; package shared; message Money { string currency = 1; }`),
		})
		srv.CreateNamespace(t, childNS)
		srv.SetNamespaceParent(t, childNS, parentNS)
		srv.PublishAndPromote(t, childNS, "billing", map[string][]byte{
			"billing.proto": []byte(billingV1),
		})

		cacheDir := t.TempDir()
		r, err := client.New(t.Context(), srv.Conn, childNS,
			client.WithRefreshInterval(0),
			client.WithServerChain(),
			client.WithDiskCache(cacheDir),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		for _, ns := range []string{childNS, parentNS} {
			_, err := os.Stat(filepath.Join(cacheDir, ns, "manifest.json"))
			require.NoError(t, err, "manifest must exist for namespace %s", ns)
		}

		// #nosec G304 -- cacheDir is t.TempDir(); not attacker-controlled.
		manBytes, err := os.ReadFile(filepath.Join(cacheDir, childNS, "manifest.json"))
		require.NoError(t, err)
		assert.Contains(t, string(manBytes), parentNS,
			"top-level manifest must record the ancestor chain for offline reload")
	})
}
