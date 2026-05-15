// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package compiler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests covering phase 2a: N-tier parent-chain resolution in
// buildResolverChain + D2 FQN-collision detection. See
// docs/design/namespace-hierarchy.md.

func TestCompile_ParentChain_FilenameResolves(t *testing.T) {
	// Child imports a file that lives only in a parent tier; the chain
	// walk should resolve it.
	c := New()

	parentFiles := []DepSource{
		{
			Namespace: "acme-shared",
			SchemaID:  "commons",
			Version:   3,
			Filename:  "acme/commons/money.proto",
			Source: []byte(`syntax = "proto3";
package acme.commons;
message Money { string currency = 1; int64 amount = 2; }
`),
		},
	}

	sources := map[string][]byte{
		"billing/invoice.proto": []byte(`syntax = "proto3";
package billing;
import "acme/commons/money.proto";
message Invoice {
  string id = 1;
  acme.commons.Money total = 2;
}
`),
	}

	result, err := c.Compile(context.Background(), 1, sources,
		nil,
		[]ChainTier{{NamespaceID: "acme-shared", Files: parentFiles}},
		nil,
	)
	require.NoError(t, err)

	md, err := result.Snapshot.FindMessageByName("billing.Invoice")
	require.NoError(t, err)
	assert.Equal(t, 2, md.Fields().Len())
}

func TestCompile_ParentChain_RecordsCrossNamespaceDeps(t *testing.T) {
	// Files from a parent tier should be recorded in result.Deps with
	// the parent's namespace ID — that's the per-import pin (D3).
	c := New()

	parentFiles := []DepSource{
		{
			Namespace: "acme-shared",
			SchemaID:  "commons",
			Version:   7,
			Filename:  "acme/commons/money.proto",
			Source: []byte(`syntax = "proto3";
package acme.commons;
message Money { string currency = 1; }
`),
		},
	}

	sources := map[string][]byte{
		"billing/invoice.proto": []byte(`syntax = "proto3";
package billing;
import "acme/commons/money.proto";
message Invoice { acme.commons.Money total = 1; }
`),
	}

	result, err := c.Compile(context.Background(), 1, sources,
		nil,
		[]ChainTier{{NamespaceID: "acme-shared", Files: parentFiles}},
		nil,
	)
	require.NoError(t, err)

	require.Len(t, result.Deps, 1)
	dep := result.Deps[0]
	assert.Equal(t, "acme-shared", dep.DepNamespaceID,
		"DepNamespaceID must reflect the parent tier the file came from")
	assert.Equal(t, "commons", dep.DepSchemaID)
	assert.Equal(t, "acme/commons/money.proto", dep.DepFilename)
	assert.Equal(t, uint64(7), dep.DepVersion)
}

func TestCompile_ChainOrder_NearestWins(t *testing.T) {
	// When two ancestors both contribute a file with the same path, the
	// nearer tier wins (filename-based chain resolution per D7).
	c := New()

	nearParent := ChainTier{
		NamespaceID: "team-a",
		Files: []DepSource{
			{
				Namespace: "team-a", SchemaID: "common", Version: 1,
				Filename: "common/money.proto",
				Source: []byte(`syntax = "proto3";
package common;
message Money { string near = 1; }
`),
			},
		},
	}
	farParent := ChainTier{
		NamespaceID: "org-shared",
		Files: []DepSource{
			{
				Namespace: "org-shared", SchemaID: "common", Version: 99,
				Filename: "common/money.proto",
				Source: []byte(`syntax = "proto3";
package common;
message Money { string far = 1; }
`),
			},
		},
	}

	sources := map[string][]byte{
		"x.proto": []byte(`syntax = "proto3";
package x;
import "common/money.proto";
message X { common.Money m = 1; }
`),
	}

	result, err := c.Compile(context.Background(), 1, sources,
		nil,
		[]ChainTier{nearParent, farParent}, // nearest first
		nil,
	)
	require.NoError(t, err)

	// Parent-tier files aren't registered in result.Snapshot's Files
	// (only the child's own files are). To check which definition of
	// common.Money won, navigate from the child's field that references
	// it — FieldDescriptor.Message() returns the resolved MessageDescriptor
	// regardless of which file owns it.
	x, err := result.Snapshot.FindMessageByName("x.X")
	require.NoError(t, err)
	money := x.Fields().Get(0).Message()
	require.NotNil(t, money)
	require.Equal(t, 1, money.Fields().Len())
	assert.Equal(t, "near", string(money.Fields().Get(0).Name()),
		"nearest tier should win for same-path collisions")
}

func TestCompile_ChainAndBuiltinsOrdering(t *testing.T) {
	// Both parent chain and builtins are provided. The parent chain
	// resolves before builtins; builtins resolve before WKT.
	c := New()

	parentFile := DepSource{
		Namespace: "parent", SchemaID: "s", Version: 1,
		Filename: "shadowed.proto",
		Source: []byte(`syntax = "proto3";
package shadowed;
message Shadowed { string from_parent = 1; }
`),
	}
	builtinFile := DepSource{
		// The literal "__builtins__" is the reserved namespace name defined
		// in the top-level package's BuiltinsNamespace constant. Hardcoded
		// here to avoid an import cycle (the registry package imports
		// compiler). The exact value is informational — the resolver
		// doesn't read it; only the conflict-attribution path does.
		Namespace: "__builtins__", SchemaID: "s", Version: 1,
		Filename: "shadowed.proto",
		Source: []byte(`syntax = "proto3";
package shadowed;
message Shadowed { string from_builtins = 1; }
`),
	}

	sources := map[string][]byte{
		"caller.proto": []byte(`syntax = "proto3";
package caller;
import "shadowed.proto";
message Caller { shadowed.Shadowed s = 1; }
`),
	}

	result, err := c.Compile(context.Background(), 1, sources,
		nil,
		[]ChainTier{{NamespaceID: "parent", Files: []DepSource{parentFile}}},
		[]DepSource{builtinFile},
	)
	require.NoError(t, err)

	// Same approach as TestCompile_ChainOrder_NearestWins: parent files
	// aren't in result.Snapshot's Files, so navigate from the child's
	// field to the referenced MessageDescriptor.
	caller, err := result.Snapshot.FindMessageByName("caller.Caller")
	require.NoError(t, err)
	shadowed := caller.Fields().Get(0).Message()
	require.NotNil(t, shadowed)
	require.Equal(t, 1, shadowed.Fields().Len())
	assert.Equal(t, "from_parent", string(shadowed.Fields().Get(0).Name()),
		"parent chain should resolve before builtins")
}

func TestCompile_EmptyParentChain_BehavesAsBefore(t *testing.T) {
	// Passing nil/empty parentChain must produce identical behavior to
	// the pre-2a path (regression guard for backward compat).
	c := New()
	sources := map[string][]byte{
		"a.proto": []byte(`syntax = "proto3"; package a; message A { string s = 1; }`),
	}
	result, err := c.Compile(context.Background(), 1, sources, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, result.Snapshot)

	// Same call with an empty (not nil) parentChain slice.
	result2, err := c.Compile(context.Background(), 1, sources, nil, []ChainTier{}, nil)
	require.NoError(t, err)
	require.NotNil(t, result2.Snapshot)

	// Equivalence of side effects: zero parent-chain deps recorded.
	assert.Empty(t, result.Deps)
	assert.Empty(t, result2.Deps)
}

// ---------------------------------------------------------------------------
// D2: FQN-collision detection
// ---------------------------------------------------------------------------

func TestDetectFQNConflicts_NoCollision(t *testing.T) {
	c := New()

	childSrc := map[string][]byte{
		"child.proto": []byte(`syntax = "proto3"; package child; message ChildMsg {}`),
	}
	parentSrc := map[string][]byte{
		"parent.proto": []byte(`syntax = "proto3"; package parent; message ParentMsg {}`),
	}

	child, err := c.Compile(context.Background(), 1, childSrc, nil, nil, nil)
	require.NoError(t, err)
	parent, err := c.Compile(context.Background(), 1, parentSrc, nil, nil, nil)
	require.NoError(t, err)

	conflicts := DetectFQNConflicts(child.Snapshot, parent.Snapshot)
	assert.Empty(t, conflicts, "no overlapping FQNs → no conflicts")
}

func TestDetectFQNConflicts_MessageCollision(t *testing.T) {
	c := New()

	// Both define acme.Money but in different files. This is the silent-
	// drift case D2 is meant to catch.
	childSrc := map[string][]byte{
		"child/money.proto": []byte(`syntax = "proto3"; package acme; message Money { string c = 1; }`),
	}
	parentSrc := map[string][]byte{
		"parent/money.proto": []byte(`syntax = "proto3"; package acme; message Money { int64 cents = 1; }`),
	}

	child, err := c.Compile(context.Background(), 1, childSrc, nil, nil, nil)
	require.NoError(t, err)
	parent, err := c.Compile(context.Background(), 1, parentSrc, nil, nil, nil)
	require.NoError(t, err)

	conflicts := DetectFQNConflicts(child.Snapshot, parent.Snapshot)
	require.Len(t, conflicts, 1)
	assert.Equal(t, "acme.Money", conflicts[0].FQN)
	assert.Equal(t, "message", conflicts[0].Kind)
	assert.Equal(t, "child/money.proto", conflicts[0].ChildFile)
	assert.Equal(t, "parent/money.proto", conflicts[0].AncestorFile)
}

func TestDetectFQNConflicts_EnumCollision(t *testing.T) {
	c := New()
	childSrc := map[string][]byte{
		"c.proto": []byte(`syntax = "proto3"; package x;
enum Status { STATUS_UNKNOWN = 0; STATUS_OK_CHILD = 1; }
`),
	}
	parentSrc := map[string][]byte{
		"p.proto": []byte(`syntax = "proto3"; package x;
enum Status { STATUS_UNKNOWN = 0; STATUS_OK_PARENT = 1; }
`),
	}

	child, err := c.Compile(context.Background(), 1, childSrc, nil, nil, nil)
	require.NoError(t, err)
	parent, err := c.Compile(context.Background(), 1, parentSrc, nil, nil, nil)
	require.NoError(t, err)

	conflicts := DetectFQNConflicts(child.Snapshot, parent.Snapshot)
	require.Len(t, conflicts, 1)
	assert.Equal(t, "x.Status", conflicts[0].FQN)
	assert.Equal(t, "enum", conflicts[0].Kind)
}

func TestDetectFQNConflicts_NestedMessage(t *testing.T) {
	// Nested messages must be walked — collision on Outer.Inner counts.
	c := New()
	childSrc := map[string][]byte{
		"c.proto": []byte(`syntax = "proto3"; package x;
message Outer {
  message Inner { string a = 1; }
  Inner i = 1;
}
`),
	}
	parentSrc := map[string][]byte{
		"p.proto": []byte(`syntax = "proto3"; package x;
message Outer {
  message Inner { int64 b = 1; }
  Inner i = 1;
}
`),
	}

	child, err := c.Compile(context.Background(), 1, childSrc, nil, nil, nil)
	require.NoError(t, err)
	parent, err := c.Compile(context.Background(), 1, parentSrc, nil, nil, nil)
	require.NoError(t, err)

	conflicts := DetectFQNConflicts(child.Snapshot, parent.Snapshot)
	// Two collisions: x.Outer and x.Outer.Inner.
	require.Len(t, conflicts, 2)
	fqns := map[string]bool{conflicts[0].FQN: true, conflicts[1].FQN: true}
	assert.True(t, fqns["x.Outer"])
	assert.True(t, fqns["x.Outer.Inner"])
}

func TestDetectFQNConflicts_NilSnapshots(t *testing.T) {
	// Defensive: nil inputs should not panic.
	assert.Empty(t, DetectFQNConflicts(nil, nil))

	c := New()
	src := map[string][]byte{
		"a.proto": []byte(`syntax = "proto3"; package a; message A {}`),
	}
	result, err := c.Compile(context.Background(), 1, src, nil, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, DetectFQNConflicts(result.Snapshot, nil))
	assert.Empty(t, DetectFQNConflicts(nil, result.Snapshot))
}

func TestFQNConflictError_Message(t *testing.T) {
	e := &FQNConflictError{
		AncestorNamespaceID: "acme-shared",
		Conflicts: []FQNConflict{
			{FQN: "acme.Money", Kind: "message", ChildFile: "x.proto", AncestorFile: "y.proto"},
		},
	}
	msg := e.Error()
	assert.Contains(t, msg, "acme.Money")
	assert.Contains(t, msg, "acme-shared")
	assert.Contains(t, msg, "y.proto")
}
