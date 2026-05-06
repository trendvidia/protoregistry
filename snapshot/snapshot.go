// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package snapshot provides immutable, compiled descriptor sets that are
// safe for concurrent access. A Snapshot is the unit of hot-swap: readers
// hold a reference to the current snapshot, and swaps atomically replace
// the pointer. Go's GC reclaims old snapshots when no readers remain.
package snapshot

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Snapshot is an immutable, compiled view of a schema at a specific version.
// It is safe for concurrent use and must not be modified after creation.
type Snapshot struct {
	version  uint64
	files    *protoregistry.Files
	types    *protoregistry.Types
	fileList []protoreflect.FileDescriptor
}

// New creates a Snapshot from a FileDescriptorSet. This is the fast path
// used during startup recovery — no protocompile invocation needed.
func New(version uint64, fds *descriptorpb.FileDescriptorSet) (*Snapshot, error) {
	files, err := protodesc.NewFiles(fds)
	if err != nil {
		return nil, fmt.Errorf("building file registry: %w", err)
	}

	types := new(protoregistry.Types)
	var fileList []protoreflect.FileDescriptor
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		fileList = append(fileList, fd)
		if err = registerTypes(types, fd); err != nil {
			return false
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("registering types: %w", err)
	}

	return &Snapshot{
		version:  version,
		files:    files,
		types:    types,
		fileList: fileList,
	}, nil
}

// NewFromFiles creates a Snapshot from already-resolved file descriptors.
// This is used after protocompile compilation.
func NewFromFiles(version uint64, fds []protoreflect.FileDescriptor) (*Snapshot, error) {
	files := new(protoregistry.Files)
	types := new(protoregistry.Types)

	for _, fd := range fds {
		if err := files.RegisterFile(fd); err != nil {
			return nil, fmt.Errorf("registering file %s: %w", fd.Path(), err)
		}
		if err := registerTypes(types, fd); err != nil {
			return nil, fmt.Errorf("registering types for %s: %w", fd.Path(), err)
		}
	}

	return &Snapshot{
		version:  version,
		files:    files,
		types:    types,
		fileList: fds,
	}, nil
}

// Version returns the schema version this snapshot represents.
func (s *Snapshot) Version() uint64 { return s.version }

// Files returns the file descriptor registry.
func (s *Snapshot) Files() *protoregistry.Files { return s.files }

// Types returns the type registry for dynamic message creation.
func (s *Snapshot) Types() *protoregistry.Types { return s.types }

// FileDescriptors returns the list of file descriptors in this snapshot.
func (s *Snapshot) FileDescriptors() []protoreflect.FileDescriptor { return s.fileList }

// FindMessageByName looks up a message descriptor by its fully-qualified name.
func (s *Snapshot) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageDescriptor, error) {
	mt, err := s.types.FindMessageByName(name)
	if err != nil {
		return nil, fmt.Errorf("message %s not found: %w", name, err)
	}
	return mt.Descriptor(), nil
}

// NewMessage creates a new dynamic message instance by fully-qualified name.
func (s *Snapshot) NewMessage(name protoreflect.FullName) (*dynamicpb.Message, error) {
	mt, err := s.types.FindMessageByName(name)
	if err != nil {
		return nil, fmt.Errorf("message %s not found: %w", name, err)
	}
	return dynamicpb.NewMessage(mt.Descriptor()), nil
}

// registerTypes registers all message and enum types from a file descriptor.
func registerTypes(types *protoregistry.Types, fd protoreflect.FileDescriptor) error {
	for i := range fd.Messages().Len() {
		if err := registerMessage(types, fd.Messages().Get(i)); err != nil {
			return err
		}
	}
	for i := range fd.Enums().Len() {
		if err := types.RegisterEnum(dynamicpb.NewEnumType(fd.Enums().Get(i))); err != nil {
			return err
		}
	}
	for i := range fd.Extensions().Len() {
		if err := types.RegisterExtension(dynamicpb.NewExtensionType(fd.Extensions().Get(i))); err != nil {
			return err
		}
	}
	return nil
}

func registerMessage(types *protoregistry.Types, md protoreflect.MessageDescriptor) error {
	if err := types.RegisterMessage(dynamicpb.NewMessageType(md)); err != nil {
		return err
	}
	for i := range md.Messages().Len() {
		if err := registerMessage(types, md.Messages().Get(i)); err != nil {
			return err
		}
	}
	for i := range md.Enums().Len() {
		if err := types.RegisterEnum(dynamicpb.NewEnumType(md.Enums().Get(i))); err != nil {
			return err
		}
	}
	for i := range md.Extensions().Len() {
		if err := types.RegisterExtension(dynamicpb.NewExtensionType(md.Extensions().Get(i))); err != nil {
			return err
		}
	}
	return nil
}
