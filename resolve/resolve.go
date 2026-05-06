// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package resolve bridges the protoregistry namespace snapshots with
// protobuf-go's protoregistry types, enabling runtime type resolution
// for external consumers.
//
// A Resolver wraps a namespace and exposes its current compiled descriptors
// through the standard protobuf-go interfaces (protodesc.Resolver,
// protoregistry.MessageTypeResolver, etc.). When a snapshot is hot-swapped,
// the resolver automatically sees the new types.
package resolve

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/trendvidia/protoregistry/namespace"
)

// Resolver provides type resolution for a specific namespace by
// delegating to the current snapshots. It implements the standard
// protobuf-go resolver interfaces so it can be used with dynamicpb,
// encoding/protojson, etc.
//
// The resolver is live: it always reads the current snapshot, so
// hot-swaps are immediately reflected without recreating the resolver.
type Resolver struct {
	ns *namespace.Namespace
}

// NewResolver creates a new Resolver for the given namespace.
func NewResolver(ns *namespace.Namespace) *Resolver {
	return &Resolver{ns: ns}
}

// FindFileByPath searches all current schemas in the namespace for a
// file with the given path.
func (r *Resolver) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	for _, snap := range r.ns.AllCurrent() {
		if fd, err := snap.Files().FindFileByPath(path); err == nil {
			return fd, nil
		}
	}
	return nil, protoregistry.NotFound
}

// FindDescriptorByName searches all current schemas in the namespace
// for a descriptor with the given full name.
func (r *Resolver) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	for _, snap := range r.ns.AllCurrent() {
		if d, err := snap.Files().FindDescriptorByName(name); err == nil {
			return d, nil
		}
	}
	return nil, protoregistry.NotFound
}

// FindMessageByName returns the message type for the given name.
// The returned type can be used to create dynamic messages.
func (r *Resolver) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	for _, snap := range r.ns.AllCurrent() {
		if mt, err := snap.Types().FindMessageByName(name); err == nil {
			return mt, nil
		}
	}
	return nil, protoregistry.NotFound
}

// FindMessageByURL looks up a message type by its URL (e.g.,
// "type.googleapis.com/acme.Config"). This enables use with
// protobuf Any types.
func (r *Resolver) FindMessageByURL(url string) (protoreflect.MessageType, error) {
	for _, snap := range r.ns.AllCurrent() {
		if mt, err := snap.Types().FindMessageByURL(url); err == nil {
			return mt, nil
		}
	}
	return nil, protoregistry.NotFound
}

// FindExtensionByName looks up an extension by name.
func (r *Resolver) FindExtensionByName(name protoreflect.FullName) (protoreflect.ExtensionType, error) {
	for _, snap := range r.ns.AllCurrent() {
		if ext, err := snap.Types().FindExtensionByName(name); err == nil {
			return ext, nil
		}
	}
	return nil, protoregistry.NotFound
}

// FindExtensionByNumber looks up an extension by the message it extends
// and its field number.
func (r *Resolver) FindExtensionByNumber(message protoreflect.FullName, field protoreflect.FieldNumber) (protoreflect.ExtensionType, error) {
	for _, snap := range r.ns.AllCurrent() {
		if ext, err := snap.Types().FindExtensionByNumber(message, field); err == nil {
			return ext, nil
		}
	}
	return nil, protoregistry.NotFound
}

// NewMessage creates a dynamic message by fully-qualified name,
// searching across all schemas in the namespace.
func (r *Resolver) NewMessage(name protoreflect.FullName) (*dynamicpb.Message, error) {
	mt, err := r.FindMessageByName(name)
	if err != nil {
		return nil, fmt.Errorf("message %s not found in namespace %s: %w", name, r.ns.ID(), err)
	}
	return dynamicpb.NewMessage(mt.Descriptor()), nil
}

// SchemaResolver provides type resolution scoped to a single schema
// within a namespace. Use this when you need to resolve types from
// a specific schema rather than the entire namespace.
type SchemaResolver struct {
	ns       *namespace.Namespace
	schemaID string
}

// NewSchemaResolver creates a resolver scoped to a specific schema.
func NewSchemaResolver(ns *namespace.Namespace, schemaID string) *SchemaResolver {
	return &SchemaResolver{ns: ns, schemaID: schemaID}
}

// FindMessageByName returns the message type from the specific schema.
func (r *SchemaResolver) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	snap := r.ns.Current(r.schemaID)
	if snap == nil {
		return nil, fmt.Errorf("schema %s has no current version", r.schemaID)
	}
	return snap.Types().FindMessageByName(name)
}

// NewMessage creates a dynamic message from the specific schema.
func (r *SchemaResolver) NewMessage(name protoreflect.FullName) (*dynamicpb.Message, error) {
	mt, err := r.FindMessageByName(name)
	if err != nil {
		return nil, fmt.Errorf("message %s not found in schema %s: %w", name, r.schemaID, err)
	}
	return dynamicpb.NewMessage(mt.Descriptor()), nil
}
