// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package namespace provides the isolation boundary for schemas.
// Each namespace maintains independent current and staged snapshots,
// with lock-free reads via atomic pointers and serialized writes.
package namespace

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/trendvidia/protoregistry/snapshot"
)

// Registry manages all namespaces. It is the top-level entry point
// for looking up and creating namespaces.
type Registry struct {
	namespaces sync.Map // namespace ID → *Namespace
}

// NewRegistry creates a new namespace registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Get returns the namespace with the given ID, or nil if it doesn't exist.
func (r *Registry) Get(id string) *Namespace {
	v, ok := r.namespaces.Load(id)
	if !ok {
		return nil
	}
	return v.(*Namespace)
}

// GetOrCreate returns an existing namespace or creates a new one.
func (r *Registry) GetOrCreate(id string) *Namespace {
	v, _ := r.namespaces.LoadOrStore(id, newNamespace(id))
	return v.(*Namespace)
}

// Range iterates over all namespaces.
func (r *Registry) Range(fn func(ns *Namespace) bool) {
	r.namespaces.Range(func(_, v any) bool {
		return fn(v.(*Namespace))
	})
}

// Namespace is an isolation boundary containing schemas.
//
// Schemas within a namespace can import each other's files; cross-namespace
// imports are not allowed. However, type *resolution* may walk the
// namespace hierarchy via the parent pointer (see SetParent / Parent /
// Chain). Resolution chaining is distinct from imports: the chain is
// consulted on lookup miss, but no source file ever names another
// namespace. See docs/design/namespace-hierarchy.md for the full design.
//
// Phase 1 ships the parent pointer and chain accessor; phase 2 wires the
// compiler and resolver to actually walk the chain.
type Namespace struct {
	id      string
	parent  atomic.Pointer[Namespace]
	schemas sync.Map // schema ID → *schemaSlot
}

// chainMaxDepth bounds the parent walk in Chain() as a safety guard. The
// store layer enforces acyclicity (see SetNamespaceParent in
// store/postgres/queries/namespaces.sql), but the in-memory model should
// never spin forever if a cycle somehow lands in memory (e.g. a bug in a
// future rehydration path). 64 is far beyond any reasonable hierarchy.
const chainMaxDepth = 64

func newNamespace(id string) *Namespace {
	return &Namespace{id: id}
}

// ID returns the namespace identifier.
func (ns *Namespace) ID() string { return ns.id }

// Parent returns the namespace's parent in the resolution chain, or nil
// when this namespace is a root (resolution then falls back to the
// implicit __builtins__ namespace and then Google WKT — see decision D4).
// Safe for concurrent use.
func (ns *Namespace) Parent() *Namespace { return ns.parent.Load() }

// SetParent atomically sets the resolution-chain parent. Pass nil to
// clear it. Safe for concurrent use, but callers are responsible for
// preventing cycles — the in-memory model trusts whatever the store
// layer admitted (see SetNamespaceParent in the store package, which
// enforces acyclicity via recursive CTE).
func (ns *Namespace) SetParent(parent *Namespace) { ns.parent.Store(parent) }

// Chain returns the resolution chain starting with this namespace and
// walking parent pointers to the root. The slice is ordered nearest-first
// (this namespace at index 0). Walks are bounded by chainMaxDepth as a
// defensive guard against cycles; the implicit __builtins__ and Google
// WKT tiers are not included (they are appended by the resolver).
//
// Safe for concurrent use; the snapshot reflects parent pointers as seen
// at the moment of each Load.
func (ns *Namespace) Chain() []*Namespace {
	chain := make([]*Namespace, 0, 4)
	seen := make(map[*Namespace]struct{}, 4)
	for cur := ns; cur != nil && len(chain) < chainMaxDepth; cur = cur.parent.Load() {
		if _, dup := seen[cur]; dup {
			break // cycle guard — should be unreachable given store-layer checks
		}
		seen[cur] = struct{}{}
		chain = append(chain, cur)
	}
	return chain
}

// Current returns the current snapshot for a schema, or nil if no
// version has been promoted.
func (ns *Namespace) Current(schemaID string) *snapshot.Snapshot {
	slot := ns.getSlot(schemaID)
	if slot == nil {
		return nil
	}
	return slot.current.Load()
}

// Staged returns the staged snapshot for a schema, or nil if nothing
// is staged.
func (ns *Namespace) Staged(schemaID string) *snapshot.Snapshot {
	slot := ns.getSlot(schemaID)
	if slot == nil {
		return nil
	}
	return slot.staged.Load()
}

// SetCurrent atomically swaps the current snapshot for a schema.
// Returns the previous snapshot (may be nil).
func (ns *Namespace) SetCurrent(schemaID string, snap *snapshot.Snapshot) *snapshot.Snapshot {
	slot := ns.getOrCreateSlot(schemaID)
	return slot.current.Swap(snap)
}

// SetStaged sets the staged snapshot for a schema.
func (ns *Namespace) SetStaged(schemaID string, snap *snapshot.Snapshot) {
	slot := ns.getOrCreateSlot(schemaID)
	slot.staged.Store(snap)
}

// Promote atomically moves all staged snapshots to current.
// Returns the schema IDs that were promoted.
func (ns *Namespace) Promote() []string {
	var promoted []string
	ns.schemas.Range(func(key, value any) bool {
		slot := value.(*schemaSlot)
		staged := slot.staged.Swap(nil)
		if staged != nil {
			slot.current.Store(staged)
			promoted = append(promoted, key.(string))
		}
		return true
	})
	return promoted
}

// DiscardStaging clears all staged snapshots.
func (ns *Namespace) DiscardStaging() {
	ns.schemas.Range(func(_, value any) bool {
		slot := value.(*schemaSlot)
		slot.staged.Store(nil)
		return true
	})
}

// AllCurrent returns a map of schema ID → current snapshot for all
// schemas that have a current version. Used for building the namespace
// resolver during compilation.
func (ns *Namespace) AllCurrent() map[string]*snapshot.Snapshot {
	result := make(map[string]*snapshot.Snapshot)
	ns.schemas.Range(func(key, value any) bool {
		slot := value.(*schemaSlot)
		if snap := slot.current.Load(); snap != nil {
			result[key.(string)] = snap
		}
		return true
	})
	return result
}

// ProposedView returns the proposed state: staged if available, otherwise
// current. This is the view used for compiling against the staging environment.
func (ns *Namespace) ProposedView() map[string]*snapshot.Snapshot {
	result := make(map[string]*snapshot.Snapshot)
	ns.schemas.Range(func(key, value any) bool {
		slot := value.(*schemaSlot)
		if snap := slot.staged.Load(); snap != nil {
			result[key.(string)] = snap
		} else if snap := slot.current.Load(); snap != nil {
			result[key.(string)] = snap
		}
		return true
	})
	return result
}

// SchemaIDs returns all schema IDs in this namespace.
func (ns *Namespace) SchemaIDs() []string {
	var ids []string
	ns.schemas.Range(func(key, _ any) bool {
		ids = append(ids, key.(string))
		return true
	})
	return ids
}

// String returns a human-readable representation.
func (ns *Namespace) String() string {
	return fmt.Sprintf("namespace(%s)", ns.id)
}

func (ns *Namespace) getSlot(schemaID string) *schemaSlot {
	v, ok := ns.schemas.Load(schemaID)
	if !ok {
		return nil
	}
	return v.(*schemaSlot)
}

func (ns *Namespace) getOrCreateSlot(schemaID string) *schemaSlot {
	v, _ := ns.schemas.LoadOrStore(schemaID, &schemaSlot{})
	return v.(*schemaSlot)
}

// schemaSlot holds the current and staged snapshots for a single schema.
// Reads are lock-free via atomic.Pointer. Writes (publish, promote) are
// serialized at the store/transaction level, not here.
type schemaSlot struct {
	current atomic.Pointer[snapshot.Snapshot]
	staged  atomic.Pointer[snapshot.Snapshot]
}
