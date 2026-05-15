// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package compiler

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protoregistry/snapshot"
)

// FQNConflict describes a fully-qualified-name collision between a child
// and an ancestor in the namespace hierarchy. See decision D2 in
// docs/design/namespace-hierarchy.md.
type FQNConflict struct {
	// FQN is the fully-qualified name that is defined in both snapshots.
	FQN string
	// Kind is what the FQN refers to: "message", "enum", "service",
	// "extension". For collisions where the two definitions are of
	// different kinds, Kind is "message" if either side is a message,
	// otherwise the child's kind.
	Kind string
	// ChildFile is the file in the child snapshot that defines the symbol.
	ChildFile string
	// AncestorFile is the file in the ancestor snapshot that defines it.
	AncestorFile string
}

// FQNConflictError aggregates one or more FQN collisions between a child
// snapshot and an ancestor. Callers (typically Registry.Publish) wrap or
// propagate this to surface a clear diagnostic.
type FQNConflictError struct {
	AncestorNamespaceID string
	Conflicts           []FQNConflict
}

func (e *FQNConflictError) Error() string {
	if len(e.Conflicts) == 1 {
		c := e.Conflicts[0]
		return fmt.Sprintf("FQN %q (%s) is already defined in namespace %q at %s",
			c.FQN, c.Kind, e.AncestorNamespaceID, c.AncestorFile)
	}
	names := make([]string, 0, len(e.Conflicts))
	for _, c := range e.Conflicts {
		names = append(names, c.FQN)
	}
	return fmt.Sprintf("%d FQNs in namespace %q conflict with ancestor %q: %v",
		len(e.Conflicts), e.Conflicts[0].FQN, e.AncestorNamespaceID, names)
}

// DetectFQNConflicts walks every type defined in the child snapshot and
// returns those whose fully-qualified name is also defined in the ancestor
// snapshot **in a different file**. Same-file collisions are not flagged:
// they represent the legitimate case where the child imports a parent
// file, and the parent's types appear in the child's snapshot via that
// import — the definition is shared, not shadowed. A real D2 conflict
// (decision D2) is when the child redefines a parent FQN in one of its
// own files.
//
// The check covers messages (including nested), enums (including nested),
// services, and top-level extensions. Method names within services are
// scoped under their service's FQN by protoreflect, so per-RPC collisions
// surface naturally through service-level FQN collisions.
//
// Conflicts are returned in deterministic order (sorted by FQN) so error
// messages are stable across runs — important for golden tests.
func DetectFQNConflicts(child, ancestor *snapshot.Snapshot) []FQNConflict {
	if child == nil || ancestor == nil {
		return nil
	}
	ancestorFQNs := collectFQNs(ancestor)
	if len(ancestorFQNs) == 0 {
		return nil
	}
	var conflicts []FQNConflict
	for fqn, cd := range collectFQNs(child) {
		ad, ok := ancestorFQNs[fqn]
		if !ok {
			continue
		}
		if cd.file == ad.file {
			// Same file path → the child is just importing the ancestor's
			// file. Not a shadow.
			continue
		}
		conflicts = append(conflicts, FQNConflict{
			FQN:          fqn,
			Kind:         cd.kind,
			ChildFile:    cd.file,
			AncestorFile: ad.file,
		})
	}
	sort.Slice(conflicts, func(i, j int) bool {
		return conflicts[i].FQN < conflicts[j].FQN
	})
	return conflicts
}

// fqnEntry is the per-FQN metadata the conflict detector tracks. Kept
// internal because callers only need the public FQNConflict type.
type fqnEntry struct {
	kind string
	file string
}

func collectFQNs(snap *snapshot.Snapshot) map[string]fqnEntry {
	out := make(map[string]fqnEntry)
	for _, fd := range snap.FileDescriptors() {
		walkFile(fd, out)
	}
	return out
}

func walkFile(fd protoreflect.FileDescriptor, out map[string]fqnEntry) {
	path := fd.Path()
	for i := range fd.Messages().Len() {
		walkMessage(fd.Messages().Get(i), path, out)
	}
	for i := range fd.Enums().Len() {
		e := fd.Enums().Get(i)
		out[string(e.FullName())] = fqnEntry{kind: "enum", file: path}
	}
	for i := range fd.Services().Len() {
		s := fd.Services().Get(i)
		out[string(s.FullName())] = fqnEntry{kind: "service", file: path}
	}
	for i := range fd.Extensions().Len() {
		ext := fd.Extensions().Get(i)
		out[string(ext.FullName())] = fqnEntry{kind: "extension", file: path}
	}
}

func walkMessage(md protoreflect.MessageDescriptor, file string, out map[string]fqnEntry) {
	out[string(md.FullName())] = fqnEntry{kind: "message", file: file}
	for i := range md.Messages().Len() {
		walkMessage(md.Messages().Get(i), file, out)
	}
	for i := range md.Enums().Len() {
		e := md.Enums().Get(i)
		out[string(e.FullName())] = fqnEntry{kind: "enum", file: file}
	}
	for i := range md.Extensions().Len() {
		ext := md.Extensions().Get(i)
		out[string(ext.FullName())] = fqnEntry{kind: "extension", file: file}
	}
}
