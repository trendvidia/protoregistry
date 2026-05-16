// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	registrypb "github.com/trendvidia/protoregistry/proto/protoregistry/v1"
)

// nsSnapshot is the immutable, atomically-swappable view of every
// schema the Resolver currently tracks for one namespace. The
// namespace-wide aggregate registries (nsFiles, nsTypes) live on the
// Resolver itself, not on the snapshot, so refresh can mutate them in
// place via UpdateFile / UnregisterFile rather than rebuilding from
// scratch on every poll.
type nsSnapshot struct {
	schemas   map[string]*schemaSnapshot
	nameIndex map[protoreflect.FullName]string // FQN -> schemaID
}

// schemaSnapshot is the compiled descriptor state for one schema at one
// version. files and types are derived from the same FileDescriptorSet
// and never mutate after construction.
//
// files and types are the namespace-isolated registry types from the
// trendvidia/protobuf-go fork (see go.mod). Each schema gets its own
// pair, so concurrent lookups across schemas in the same Resolver
// never contend on a shared mutex. The fork's types satisfy the
// standard [protoreflect.MessageTypeResolver],
// [protoreflect.ExtensionTypeResolver], and [protodesc.Resolver]
// interfaces, so the rest of the package treats them generically.
//
// aggFingerprint records exactly which entries this schema contributes
// to the Resolver-level aggregate registries. On refresh, when this
// schema is replaced (new version) or removed (deleted from the
// namespace), the fingerprint tells [Resolver.removeFromAggregate]
// what to call UnregisterFile / UnregisterMessage / UnregisterEnum /
// UnregisterExtension on. Captured once during fetchSchema so the
// removal path doesn't have to walk descriptors a second time.
type schemaSnapshot struct {
	schemaID       string
	version        uint64
	files          *protoregistry.NamespacedFiles
	types          *protoregistry.NamespacedTypes
	aggFingerprint aggFingerprint

	// rawDescriptorSet is the wire-format FileDescriptorSet bytes
	// this snapshot was built from. Populated when fetched via
	// GetDescriptor or loaded from a disk cache; the disk-cache
	// persister writes these bytes verbatim, and the cache loader
	// re-compiles them through the same path as fetchSchema.
	// Empty for snapshots that pre-date the cache feature (won't
	// be persisted but won't crash either).
	rawDescriptorSet []byte
}

// aggFingerprint is the set of identifiers a schema contributes to the
// namespace-wide aggregate registries. Stored on schemaSnapshot at
// fetch time; consumed by removeFromAggregate at refresh time.
type aggFingerprint struct {
	filePaths  []string
	messages   []protoreflect.FullName
	enums      []protoreflect.FullName
	extensions []protoreflect.FullName
}

func newSnapshot(sizeHint int) *nsSnapshot {
	return &nsSnapshot{
		schemas:   make(map[string]*schemaSnapshot, sizeHint),
		nameIndex: make(map[protoreflect.FullName]string, sizeHint*8),
	}
}

// applyToAggregate folds ss's files and types into the Resolver-level
// nsFiles / nsTypes. Uses Update* (upsert) so duplicate file paths or
// extension numbers across schemas resolve last-wins — matches the
// pre-incremental-refresh semantics. Cross-schema FQN collisions on
// messages/enums are caught separately by [nsSnapshot.buildNameIndex]'s
// fail-loud check.
func (r *Resolver) applyToAggregate(ss *schemaSnapshot) error {
	var rangeErr error
	ss.files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if err := r.nsFiles.UpdateFile(fd); err != nil {
			rangeErr = fmt.Errorf("aggregating file %s from schema %s: %w", fd.Path(), ss.schemaID, err)
			return false
		}
		if err := registerFileTypesUpdate(r.nsTypes, fd); err != nil {
			rangeErr = fmt.Errorf("aggregating types from %s in schema %s: %w", fd.Path(), ss.schemaID, err)
			return false
		}
		return true
	})
	return rangeErr
}

// removeFromAggregate undoes a previous applyToAggregate for ss.
// Uses ss.aggFingerprint (captured during fetchSchema) to call
// UnregisterFile / UnregisterMessage / UnregisterEnum /
// UnregisterExtension on exactly the entries this schema contributed —
// no descriptor walking required.
//
// Errors from Unregister* indicate a fingerprint that disagreed with
// the aggregate's actual state, which would be a programming bug. They
// are wrapped and returned; the caller decides whether to panic, log,
// or surface.
func (r *Resolver) removeFromAggregate(ss *schemaSnapshot) error {
	for _, path := range ss.aggFingerprint.filePaths {
		if err := r.nsFiles.UnregisterFile(path); err != nil {
			return fmt.Errorf("unregister file %s (schema %s): %w", path, ss.schemaID, err)
		}
	}
	for _, name := range ss.aggFingerprint.messages {
		if err := r.nsTypes.UnregisterMessage(name); err != nil {
			return fmt.Errorf("unregister message %s (schema %s): %w", name, ss.schemaID, err)
		}
	}
	for _, name := range ss.aggFingerprint.enums {
		if err := r.nsTypes.UnregisterEnum(name); err != nil {
			return fmt.Errorf("unregister enum %s (schema %s): %w", name, ss.schemaID, err)
		}
	}
	for _, name := range ss.aggFingerprint.extensions {
		if err := r.nsTypes.UnregisterExtension(name); err != nil {
			return fmt.Errorf("unregister extension %s (schema %s): %w", name, ss.schemaID, err)
		}
	}
	return nil
}

// registerFileTypesUpdate is the Update* counterpart of
// registerFileTypes. Used when building the namespace-wide aggregate
// where the same descriptor (e.g. a well-known type) may already be
// registered by a sibling schema; UpdateMessage / UpdateEnum /
// UpdateExtension upsert silently rather than erroring on duplicates.
func registerFileTypesUpdate(types *protoregistry.NamespacedTypes, fd protoreflect.FileDescriptor) error {
	msgs := fd.Messages()
	for i := 0; i < msgs.Len(); i++ {
		if err := registerMessageTypesUpdate(types, msgs.Get(i)); err != nil {
			return err
		}
	}
	enums := fd.Enums()
	for i := 0; i < enums.Len(); i++ {
		if err := types.UpdateEnum(dynamicpb.NewEnumType(enums.Get(i))); err != nil {
			return err
		}
	}
	exts := fd.Extensions()
	for i := 0; i < exts.Len(); i++ {
		if err := types.UpdateExtension(dynamicpb.NewExtensionType(exts.Get(i))); err != nil {
			return err
		}
	}
	return nil
}

func registerMessageTypesUpdate(types *protoregistry.NamespacedTypes, msg protoreflect.MessageDescriptor) error {
	if err := types.UpdateMessage(dynamicpb.NewMessageType(msg)); err != nil {
		return err
	}
	nested := msg.Messages()
	for i := 0; i < nested.Len(); i++ {
		if err := registerMessageTypesUpdate(types, nested.Get(i)); err != nil {
			return err
		}
	}
	enums := msg.Enums()
	for i := 0; i < enums.Len(); i++ {
		if err := types.UpdateEnum(dynamicpb.NewEnumType(enums.Get(i))); err != nil {
			return err
		}
	}
	exts := msg.Extensions()
	for i := 0; i < exts.Len(); i++ {
		if err := types.UpdateExtension(dynamicpb.NewExtensionType(exts.Get(i))); err != nil {
			return err
		}
	}
	return nil
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
	rawBytes, err := proto.Marshal(resp.FileDescriptorSet)
	if err != nil {
		return nil, fmt.Errorf("remarshal FileDescriptorSet for %s/%s@%d: %w", r.ns, schemaID, version, err)
	}
	return r.compileSchema(schemaID, resp.Version, resp.FileDescriptorSet, rawBytes)
}

// compileSchema turns a FileDescriptorSet into a schemaSnapshot,
// registering every file into a fresh pair of NamespacedFiles /
// NamespacedTypes that inherit the Resolver's configured parents.
// rawDescriptorSet is the wire-format bytes of the same set; we
// stash them on the snapshot so the disk-cache persister can write
// them out without re-marshaling.
//
// Pulled out of fetchSchema so the disk-cache loader can construct
// snapshots from disk bytes through exactly the same code path —
// no semantic divergence between "live" and "cached" snapshots.
func (r *Resolver) compileSchema(schemaID string, version uint64, fdset *descriptorpb.FileDescriptorSet, rawDescriptorSet []byte) (*schemaSnapshot, error) {
	// Compile the wire FileDescriptorSet via protodesc to resolve
	// cross-file dependencies, then transfer the result into a fresh
	// pair of NamespacedFiles / NamespacedTypes registries owned by
	// this schema. The intermediate *protoregistry.Files is dropped
	// once registration completes.
	compiled, err := protodesc.NewFiles(fdset)
	if err != nil {
		return nil, fmt.Errorf("compiling descriptors for %s/%s@%d: %w", r.ns, schemaID, version, err)
	}

	// Per-schema views inherit the same parent as the namespace-wide
	// aggregate (configured via WithFallback / WithParent /
	// WithGlobalFallback), so SchemaResolver lookups also see
	// well-known and shared types declared at the parent level.
	files := protoregistry.NewNamespacedFiles(r.cfg.parentFiles)
	types := protoregistry.NewNamespacedTypes(r.cfg.parentTypes)
	var fp aggFingerprint
	var rangeErr error
	compiled.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if err := files.RegisterFile(fd); err != nil {
			rangeErr = fmt.Errorf("register file %s: %w", fd.Path(), err)
			return false
		}
		if err := registerFileTypes(types, fd); err != nil {
			rangeErr = fmt.Errorf("register types in %s: %w", fd.Path(), err)
			return false
		}
		fp.collect(fd)
		return true
	})
	if rangeErr != nil {
		return nil, fmt.Errorf("populating registries for %s/%s@%d: %w", r.ns, schemaID, version, rangeErr)
	}

	return &schemaSnapshot{
		schemaID:         schemaID,
		version:          version,
		files:            files,
		types:            types,
		aggFingerprint:   fp,
		rawDescriptorSet: rawDescriptorSet,
	}, nil
}

// collect walks fd and records every file path and named descriptor in
// the fingerprint. Mirrors the work registerFileTypes does for the
// per-schema NamespacedTypes, so a later removeFromAggregate has the
// exact set of identifiers to unregister from the namespace-wide
// aggregate.
func (fp *aggFingerprint) collect(fd protoreflect.FileDescriptor) {
	fp.filePaths = append(fp.filePaths, fd.Path())
	msgs := fd.Messages()
	for i := 0; i < msgs.Len(); i++ {
		fp.collectMessage(msgs.Get(i))
	}
	enums := fd.Enums()
	for i := 0; i < enums.Len(); i++ {
		fp.enums = append(fp.enums, enums.Get(i).FullName())
	}
	exts := fd.Extensions()
	for i := 0; i < exts.Len(); i++ {
		fp.extensions = append(fp.extensions, exts.Get(i).FullName())
	}
}

func (fp *aggFingerprint) collectMessage(msg protoreflect.MessageDescriptor) {
	fp.messages = append(fp.messages, msg.FullName())
	nested := msg.Messages()
	for i := 0; i < nested.Len(); i++ {
		fp.collectMessage(nested.Get(i))
	}
	enums := msg.Enums()
	for i := 0; i < enums.Len(); i++ {
		fp.enums = append(fp.enums, enums.Get(i).FullName())
	}
	exts := msg.Extensions()
	for i := 0; i < exts.Len(); i++ {
		fp.extensions = append(fp.extensions, exts.Get(i).FullName())
	}
}

// registerFileTypes walks every named, instantiable descriptor in fd
// (messages and their nested messages, enums, extensions) and
// registers a dynamic type for it in types. Services and methods do
// not have associated runtime types, so they are skipped here — they
// are still discoverable via the parallel files registry.
func registerFileTypes(types *protoregistry.NamespacedTypes, fd protoreflect.FileDescriptor) error {
	msgs := fd.Messages()
	for i := 0; i < msgs.Len(); i++ {
		if err := registerMessageTypes(types, msgs.Get(i)); err != nil {
			return err
		}
	}
	enums := fd.Enums()
	for i := 0; i < enums.Len(); i++ {
		if err := types.RegisterEnum(dynamicpb.NewEnumType(enums.Get(i))); err != nil {
			return err
		}
	}
	exts := fd.Extensions()
	for i := 0; i < exts.Len(); i++ {
		if err := types.RegisterExtension(dynamicpb.NewExtensionType(exts.Get(i))); err != nil {
			return err
		}
	}
	return nil
}

func registerMessageTypes(types *protoregistry.NamespacedTypes, msg protoreflect.MessageDescriptor) error {
	if err := types.RegisterMessage(dynamicpb.NewMessageType(msg)); err != nil {
		return err
	}
	nested := msg.Messages()
	for i := 0; i < nested.Len(); i++ {
		if err := registerMessageTypes(types, nested.Get(i)); err != nil {
			return err
		}
	}
	enums := msg.Enums()
	for i := 0; i < enums.Len(); i++ {
		if err := types.RegisterEnum(dynamicpb.NewEnumType(enums.Get(i))); err != nil {
			return err
		}
	}
	exts := msg.Extensions()
	for i := 0; i < exts.Len(); i++ {
		if err := types.RegisterExtension(dynamicpb.NewExtensionType(exts.Get(i))); err != nil {
			return err
		}
	}
	return nil
}

// populate runs the eager initial fetch for a freshly constructed
// Resolver: list every schema in the namespace, fetch each at its
// current version, build the name index, populate the namespace-wide
// aggregate, and install the snapshot.
//
// Subsequent updates go through [Resolver.Refresh], which mutates the
// aggregate incrementally rather than rebuilding it from scratch.
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

	// Apply schemas to the aggregate in deterministic order so the
	// last-wins resolution of any file-path or extension-number conflict
	// is reproducible across runs.
	ids := make([]string, 0, len(snap.schemas))
	for id := range snap.schemas {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := r.applyToAggregate(snap.schemas[id]); err != nil {
			return err
		}
	}

	r.snapshot.Store(snap)
	return nil
}
