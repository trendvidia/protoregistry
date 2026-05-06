// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package client_test

import (
	"testing"

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
}
