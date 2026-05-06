// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	registrypb "github.com/trendvidia/protoregistry/proto/protoregistry/v1"
)

// nsSnapshot is the immutable, atomically-swappable view of every
// schema the Resolver currently tracks for one namespace.
type nsSnapshot struct {
	schemas   map[string]*schemaSnapshot
	nameIndex map[protoreflect.FullName]string // FQN -> schemaID
}

// schemaSnapshot is the compiled descriptor state for one schema at one
// version. files and types are derived from the same FileDescriptorSet
// and never mutate after construction.
type schemaSnapshot struct {
	schemaID string
	version  uint64
	files    *protoregistry.Files
	types    *dynamicpb.Types
}

func newSnapshot(sizeHint int) *nsSnapshot {
	return &nsSnapshot{
		schemas:   make(map[string]*schemaSnapshot, sizeHint),
		nameIndex: make(map[protoreflect.FullName]string, sizeHint*8),
	}
}

// schemaFor returns the schema that owns the given FQN, if any.
func (s *nsSnapshot) schemaFor(name protoreflect.FullName) (*schemaSnapshot, bool) {
	id, ok := s.nameIndex[name]
	if !ok {
		return nil, false
	}
	ss, ok := s.schemas[id]
	return ss, ok
}

// buildNameIndex walks every descriptor in every schema and records a
// FQN -> schemaID mapping. Returns an error listing every collision
// when two schemas in the namespace export the same FQN — the
// "fail-loud" decision documented in [doc.go].
func (s *nsSnapshot) buildNameIndex() error {
	type conflict struct {
		name      protoreflect.FullName
		schemaIDs []string
	}
	conflicts := map[protoreflect.FullName]*conflict{}

	// Iterate schemas in deterministic order so error messages are stable
	// across runs.
	ids := make([]string, 0, len(s.schemas))
	for id := range s.schemas {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		ss := s.schemas[id]
		ss.files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
			rangeFileDescriptors(fd, func(name protoreflect.FullName) {
				if existing, ok := s.nameIndex[name]; ok {
					if existing == id {
						return
					}
					c, ok := conflicts[name]
					if !ok {
						c = &conflict{name: name, schemaIDs: []string{existing}}
						conflicts[name] = c
					}
					c.schemaIDs = append(c.schemaIDs, id)
					return
				}
				s.nameIndex[name] = id
			})
			return true
		})
	}

	if len(conflicts) == 0 {
		return nil
	}

	names := make([]string, 0, len(conflicts))
	for n := range conflicts {
		names = append(names, string(n))
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("protoregistry/client: name collisions across schemas:")
	for _, n := range names {
		c := conflicts[protoreflect.FullName(n)]
		fmt.Fprintf(&b, " %s in [%s];", n, strings.Join(c.schemaIDs, ", "))
	}
	return fmt.Errorf("%s", strings.TrimSuffix(b.String(), ";"))
}

// rangeFileDescriptors visits every named descriptor in fd: messages
// (recursively), enums, extensions, services. Methods are reachable as
// children of services via FullName, so we record them too.
func rangeFileDescriptors(fd protoreflect.FileDescriptor, fn func(protoreflect.FullName)) {
	msgs := fd.Messages()
	for i := 0; i < msgs.Len(); i++ {
		rangeMessageDescriptors(msgs.Get(i), fn)
	}
	enums := fd.Enums()
	for i := 0; i < enums.Len(); i++ {
		fn(enums.Get(i).FullName())
	}
	exts := fd.Extensions()
	for i := 0; i < exts.Len(); i++ {
		fn(exts.Get(i).FullName())
	}
	svcs := fd.Services()
	for i := 0; i < svcs.Len(); i++ {
		svc := svcs.Get(i)
		fn(svc.FullName())
		methods := svc.Methods()
		for j := 0; j < methods.Len(); j++ {
			fn(methods.Get(j).FullName())
		}
	}
}

func rangeMessageDescriptors(msg protoreflect.MessageDescriptor, fn func(protoreflect.FullName)) {
	fn(msg.FullName())
	nested := msg.Messages()
	for i := 0; i < nested.Len(); i++ {
		rangeMessageDescriptors(nested.Get(i), fn)
	}
	enums := msg.Enums()
	for i := 0; i < enums.Len(); i++ {
		fn(enums.Get(i).FullName())
	}
	exts := msg.Extensions()
	for i := 0; i < exts.Len(); i++ {
		fn(exts.Get(i).FullName())
	}
}

// fetchSchema calls GetDescriptor and compiles the returned
// FileDescriptorSet into a schemaSnapshot.
func (r *Resolver) fetchSchema(ctx context.Context, schemaID string, version uint64) (*schemaSnapshot, error) {
	resp, err := r.rpc.GetDescriptor(ctx, &registrypb.GetDescriptorRequest{
		NamespaceId: r.ns,
		SchemaId:    schemaID,
		Version:     version,
	})
	if err != nil {
		return nil, fmt.Errorf("get_descriptor %s/%s@%d: %w", r.ns, schemaID, version, err)
	}
	files, err := protodesc.NewFiles(resp.FileDescriptorSet)
	if err != nil {
		return nil, fmt.Errorf("compiling descriptors for %s/%s@%d: %w", r.ns, schemaID, version, err)
	}
	return &schemaSnapshot{
		schemaID: schemaID,
		version:  resp.Version,
		files:    files,
		types:    dynamicpb.NewTypes(files),
	}, nil
}

// populate runs the eager initial fetch for a freshly constructed
// Resolver: list every schema in the namespace, fetch each at its
// current version, build the name index, and install the snapshot.
func (r *Resolver) populate(ctx context.Context) error {
	infos, err := r.listAllSchemas(ctx)
	if err != nil {
		return fmt.Errorf("populating namespace %q: %w", r.ns, err)
	}

	snap := newSnapshot(len(infos))
	for _, info := range infos {
		if info.CurrentVersion == nil {
			continue
		}
		ss, err := r.fetchSchema(ctx, info.SchemaId, *info.CurrentVersion)
		if err != nil {
			return err
		}
		snap.schemas[info.SchemaId] = ss
	}
	if err := snap.buildNameIndex(); err != nil {
		return err
	}
	r.snapshot.Store(snap)
	return nil
}
