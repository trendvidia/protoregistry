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
//
// Resolver also walks the namespace hierarchy chain on lookup miss
// (decision D7 in docs/design/namespace-hierarchy.md): when a symbol
// is not found in the publishing namespace's current snapshots, the
// search continues through each ancestor namespace in chain order.
// The nearest tier wins. Use the *WithOrigin variants to recover which
// namespace contributed the resolved descriptor — needed for hover
// provenance in protolsp and similar tools.
type Resolver struct {
	ns *namespace.Namespace
}

// NewResolver creates a new Resolver for the given namespace.
func NewResolver(ns *namespace.Namespace) *Resolver {
	return &Resolver{ns: ns}
}

// FindFileByPath searches the namespace chain for a file with the given
// path. Equivalent to FindFileByPathWithOrigin without the namespace ID.
func (r *Resolver) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	fd, _, err := r.FindFileByPathWithOrigin(path)
	return fd, err
}

// FindFileByPathWithOrigin searches the namespace chain for a file with
// the given path and returns the namespace ID that contributed it.
func (r *Resolver) FindFileByPathWithOrigin(path string) (protoreflect.FileDescriptor, string, error) {
	for _, ns := range r.ns.Chain() {
		for _, snap := range ns.AllCurrent() {
			if fd, err := snap.Files().FindFileByPath(path); err == nil {
				return fd, ns.ID(), nil
			}
		}
	}
	return nil, "", protoregistry.NotFound
}

// FindDescriptorByName searches the namespace chain for a descriptor.
func (r *Resolver) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	d, _, err := r.FindDescriptorByNameWithOrigin(name)
	return d, err
}

// FindDescriptorByNameWithOrigin searches the namespace chain for a
// descriptor and returns the namespace ID that contributed it.
func (r *Resolver) FindDescriptorByNameWithOrigin(name protoreflect.FullName) (protoreflect.Descriptor, string, error) {
	for _, ns := range r.ns.Chain() {
		for _, snap := range ns.AllCurrent() {
			if d, err := snap.Files().FindDescriptorByName(name); err == nil {
				return d, ns.ID(), nil
			}
		}
	}
	return nil, "", protoregistry.NotFound
}

// FindMessageByName returns the message type for the given name.
// The returned type can be used to create dynamic messages.
func (r *Resolver) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	mt, _, err := r.FindMessageByNameWithOrigin(name)
	return mt, err
}

// FindMessageByNameWithOrigin searches the namespace chain for a message
// type and returns the namespace ID that contributed it.
func (r *Resolver) FindMessageByNameWithOrigin(name protoreflect.FullName) (protoreflect.MessageType, string, error) {
	for _, ns := range r.ns.Chain() {
		for _, snap := range ns.AllCurrent() {
			if mt, err := snap.Types().FindMessageByName(name); err == nil {
				return mt, ns.ID(), nil
			}
		}
	}
	return nil, "", protoregistry.NotFound
}

// FindMessageByURL looks up a message type by its URL (e.g.,
// "type.googleapis.com/acme.Config"). This enables use with
// protobuf Any types.
func (r *Resolver) FindMessageByURL(url string) (protoreflect.MessageType, error) {
	mt, _, err := r.FindMessageByURLWithOrigin(url)
	return mt, err
}

// FindMessageByURLWithOrigin is FindMessageByURL with origin namespace.
func (r *Resolver) FindMessageByURLWithOrigin(url string) (protoreflect.MessageType, string, error) {
	for _, ns := range r.ns.Chain() {
		for _, snap := range ns.AllCurrent() {
			if mt, err := snap.Types().FindMessageByURL(url); err == nil {
				return mt, ns.ID(), nil
			}
		}
	}
	return nil, "", protoregistry.NotFound
}

// FindExtensionByName looks up an extension by name.
func (r *Resolver) FindExtensionByName(name protoreflect.FullName) (protoreflect.ExtensionType, error) {
	ext, _, err := r.FindExtensionByNameWithOrigin(name)
	return ext, err
}

// FindExtensionByNameWithOrigin is FindExtensionByName with origin namespace.
func (r *Resolver) FindExtensionByNameWithOrigin(name protoreflect.FullName) (protoreflect.ExtensionType, string, error) {
	for _, ns := range r.ns.Chain() {
		for _, snap := range ns.AllCurrent() {
			if ext, err := snap.Types().FindExtensionByName(name); err == nil {
				return ext, ns.ID(), nil
			}
		}
	}
	return nil, "", protoregistry.NotFound
}

// FindExtensionByNumber looks up an extension by the message it extends
// and its field number.
func (r *Resolver) FindExtensionByNumber(message protoreflect.FullName, field protoreflect.FieldNumber) (protoreflect.ExtensionType, error) {
	ext, _, err := r.FindExtensionByNumberWithOrigin(message, field)
	return ext, err
}

// FindExtensionByNumberWithOrigin is FindExtensionByNumber with origin namespace.
func (r *Resolver) FindExtensionByNumberWithOrigin(message protoreflect.FullName, field protoreflect.FieldNumber) (protoreflect.ExtensionType, string, error) {
	for _, ns := range r.ns.Chain() {
		for _, snap := range ns.AllCurrent() {
			if ext, err := snap.Types().FindExtensionByNumber(message, field); err == nil {
				return ext, ns.ID(), nil
			}
		}
	}
	return nil, "", protoregistry.NotFound
}

// NewMessage creates a dynamic message by fully-qualified name,
// searching across the namespace chain.
func (r *Resolver) NewMessage(name protoreflect.FullName) (*dynamicpb.Message, error) {
	mt, err := r.FindMessageByName(name)
	if err != nil {
		return nil, fmt.Errorf("message %s not found in namespace %s (or any ancestor): %w", name, r.ns.ID(), err)
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
