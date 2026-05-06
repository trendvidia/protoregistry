// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package namespace

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/trendvidia/protoregistry/snapshot"
)

func makeTestSnapshot(t *testing.T, version uint64) *snapshot.Snapshot {
	t.Helper()
	fds := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{
			{
				Name:    proto.String("test.proto"),
				Package: proto.String("test"),
				Syntax:  proto.String("proto3"),
				MessageType: []*descriptorpb.DescriptorProto{
					{
						Name: proto.String("Msg"),
						Field: []*descriptorpb.FieldDescriptorProto{
							{
								Name:     proto.String("id"),
								Number:   proto.Int32(1),
								Type:     descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
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

func TestRegistryGetOrCreate(t *testing.T) {
	r := NewRegistry()

	ns1 := r.GetOrCreate("acme")
	ns2 := r.GetOrCreate("acme")
	assert.Same(t, ns1, ns2, "same namespace returned for same ID")

	ns3 := r.GetOrCreate("other")
	assert.NotSame(t, ns1, ns3, "different namespace for different ID")
}

func TestRegistryGet(t *testing.T) {
	r := NewRegistry()
	assert.Nil(t, r.Get("missing"))

	r.GetOrCreate("acme")
	assert.NotNil(t, r.Get("acme"))
}

func TestRegistryRange(t *testing.T) {
	r := NewRegistry()
	r.GetOrCreate("a")
	r.GetOrCreate("b")
	r.GetOrCreate("c")

	var ids []string
	r.Range(func(ns *Namespace) bool {
		ids = append(ids, ns.ID())
		return true
	})
	assert.Len(t, ids, 3)
}

func TestNamespaceCurrentAndStaged(t *testing.T) {
	ns := newNamespace("test")

	assert.Nil(t, ns.Current("billing"))
	assert.Nil(t, ns.Staged("billing"))

	snap1 := makeTestSnapshot(t, 1)
	ns.SetCurrent("billing", snap1)
	assert.Same(t, snap1, ns.Current("billing"))
	assert.Nil(t, ns.Staged("billing"))

	snap2 := makeTestSnapshot(t, 2)
	ns.SetStaged("billing", snap2)
	assert.Same(t, snap1, ns.Current("billing"))
	assert.Same(t, snap2, ns.Staged("billing"))
}

func TestNamespacePromote(t *testing.T) {
	ns := newNamespace("test")

	snap1 := makeTestSnapshot(t, 1)
	snap2 := makeTestSnapshot(t, 2)
	snap3 := makeTestSnapshot(t, 3)

	ns.SetCurrent("billing", snap1)
	ns.SetStaged("billing", snap2)
	ns.SetStaged("common", snap3)

	promoted := ns.Promote()
	assert.Len(t, promoted, 2)

	// Staged moved to current.
	assert.Same(t, snap2, ns.Current("billing"))
	assert.Same(t, snap3, ns.Current("common"))

	// Staged is cleared.
	assert.Nil(t, ns.Staged("billing"))
	assert.Nil(t, ns.Staged("common"))
}

func TestNamespaceDiscardStaging(t *testing.T) {
	ns := newNamespace("test")

	snap1 := makeTestSnapshot(t, 1)
	snap2 := makeTestSnapshot(t, 2)

	ns.SetCurrent("billing", snap1)
	ns.SetStaged("billing", snap2)

	ns.DiscardStaging()

	// Current unchanged, staged cleared.
	assert.Same(t, snap1, ns.Current("billing"))
	assert.Nil(t, ns.Staged("billing"))
}

func TestNamespaceProposedView(t *testing.T) {
	ns := newNamespace("test")

	snap1 := makeTestSnapshot(t, 1)
	snap2 := makeTestSnapshot(t, 2)
	snap3 := makeTestSnapshot(t, 3)

	ns.SetCurrent("billing", snap1)
	ns.SetCurrent("common", snap3)
	ns.SetStaged("billing", snap2)

	proposed := ns.ProposedView()
	assert.Same(t, snap2, proposed["billing"], "staged preferred over current")
	assert.Same(t, snap3, proposed["common"], "current used when no staged")
}

func TestNamespaceAllCurrent(t *testing.T) {
	ns := newNamespace("test")

	snap1 := makeTestSnapshot(t, 1)
	snap2 := makeTestSnapshot(t, 2)

	ns.SetCurrent("billing", snap1)
	ns.SetCurrent("common", snap2)
	ns.SetStaged("billing", makeTestSnapshot(t, 3))

	all := ns.AllCurrent()
	assert.Len(t, all, 2)
	assert.Same(t, snap1, all["billing"], "current, not staged")
	assert.Same(t, snap2, all["common"])
}

func TestNamespaceSchemaIDs(t *testing.T) {
	ns := newNamespace("test")
	ns.SetCurrent("billing", makeTestSnapshot(t, 1))
	ns.SetCurrent("common", makeTestSnapshot(t, 2))

	ids := ns.SchemaIDs()
	assert.Len(t, ids, 2)
	assert.Contains(t, ids, "billing")
	assert.Contains(t, ids, "common")
}

func TestNamespaceConcurrentSwaps(t *testing.T) {
	ns := newNamespace("test")
	ns.SetCurrent("billing", makeTestSnapshot(t, 0))

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(v int) {
			defer wg.Done()
			snap := makeTestSnapshot(t, uint64(v+1)) // #nosec G115 -- test loop counter, bounded by goroutines
			ns.SetCurrent("billing", snap)
			// Concurrent reads should never panic.
			_ = ns.Current("billing")
			_ = ns.AllCurrent()
			_ = ns.ProposedView()
		}(i)
	}
	wg.Wait()

	// After all goroutines, billing should have some snapshot.
	assert.NotNil(t, ns.Current("billing"))
}

func TestNamespaceString(t *testing.T) {
	ns := newNamespace("acme")
	assert.Equal(t, "namespace(acme)", ns.String())
}

// TestNamespaceHotSwapMonotonicity hammers the atomic.Pointer hot-swap
// path: one writer keeps Promote()-ing newer staged versions while a pool
// of readers continuously reads Current(). Two invariants:
//
//   - No reader ever sees a torn snapshot (would crash under -race).
//   - The version each reader observes is monotonically non-decreasing
//     within that reader's own observation sequence (atomic.Pointer
//     guarantees this; the test catches any regression where a reader
//     could see an older snapshot after a newer one).
//
// Run with `go test -race ./namespace/...` to exercise the race detector.
func TestNamespaceHotSwapMonotonicity(t *testing.T) {
	ns := newNamespace("test")
	ns.SetCurrent("billing", makeTestSnapshot(t, 1))

	const (
		readers      = 16
		swaps        = 500
		readsPerSwap = 64
	)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writer: repeatedly stage a higher version, then Promote.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= swaps; i++ {
			ns.SetStaged("billing", makeTestSnapshot(t, uint64(i+1)))
			ns.Promote()
		}
		close(stop)
	}()

	// Readers: Current() must never go backwards within one reader's
	// observation stream. atomic.Pointer.Load gives us this for free; the
	// test just makes sure nothing in Current() has accidentally taken a
	// value-by-copy path that would weaken the guarantee.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var prev uint64
			for {
				select {
				case <-stop:
					return
				default:
				}
				for j := 0; j < readsPerSwap; j++ {
					snap := ns.Current("billing")
					if snap == nil {
						continue
					}
					v := snap.Version()
					if v < prev {
						t.Errorf("non-monotonic snapshot version: prev=%d now=%d", prev, v)
						return
					}
					prev = v
				}
			}
		}()
	}

	wg.Wait()

	// Final Current() must reflect the last write.
	final := ns.Current("billing")
	require.NotNil(t, final)
	assert.Equal(t, uint64(swaps+1), final.Version())
}
