// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package client

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// TestBuildNameIndex_Collision verifies the fail-loud guarantee: two
// schemas in the same namespace exporting the same FQN must cause
// buildNameIndex to error rather than silently picking one.
//
// This is a unit test rather than an integration test because the
// server-side compiler tends to reject cross-schema collisions at
// publish time (gatherDeps wires every other schema as a dep, so the
// compiler sees the duplicate). The client's check is a defensive
// guard for cases where descriptors arrive from a less-coordinated
// source — it deserves direct coverage.
func TestBuildNameIndex_Collision(t *testing.T) {
	a := buildSchemaSnapshot(t, "schema-a", `
		name: "a.proto"
		package: "shared"
		syntax: "proto3"
		message_type { name: "Foo" }
	`)
	b := buildSchemaSnapshot(t, "schema-b", `
		name: "b.proto"
		package: "shared"
		syntax: "proto3"
		message_type { name: "Foo" }
	`)

	snap := newSnapshot(2)
	snap.schemas["schema-a"] = a
	snap.schemas["schema-b"] = b

	err := snap.buildNameIndex()
	if err == nil {
		t.Fatal("buildNameIndex: want error for cross-schema collision, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "shared.Foo") {
		t.Errorf("error must name the colliding FQN; got: %v", err)
	}
	if !strings.Contains(msg, "schema-a") || !strings.Contains(msg, "schema-b") {
		t.Errorf("error must list both colliding schemas; got: %v", err)
	}
}

// TestBuildNameIndex_NoCollision sanity-checks that distinct FQNs
// across schemas populate the index correctly.
func TestBuildNameIndex_NoCollision(t *testing.T) {
	a := buildSchemaSnapshot(t, "schema-a", `
		name: "a.proto"
		package: "a"
		syntax: "proto3"
		message_type { name: "Foo" }
	`)
	b := buildSchemaSnapshot(t, "schema-b", `
		name: "b.proto"
		package: "b"
		syntax: "proto3"
		message_type { name: "Foo" }
	`)

	snap := newSnapshot(2)
	snap.schemas["schema-a"] = a
	snap.schemas["schema-b"] = b

	if err := snap.buildNameIndex(); err != nil {
		t.Fatalf("buildNameIndex: unexpected error: %v", err)
	}
	if got := snap.nameIndex["a.Foo"]; got != "schema-a" {
		t.Errorf("a.Foo: got schemaID %q, want schema-a", got)
	}
	if got := snap.nameIndex["b.Foo"]; got != "schema-b" {
		t.Errorf("b.Foo: got schemaID %q, want schema-b", got)
	}
}

func buildSchemaSnapshot(t *testing.T, schemaID, fileTextProto string) *schemaSnapshot {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{}
	if err := prototext.Unmarshal([]byte(fileTextProto), fdp); err != nil {
		t.Fatalf("parsing FileDescriptorProto: %v", err)
	}
	fds := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp}}
	compiled, err := protodesc.NewFiles(fds)
	if err != nil {
		t.Fatalf("compiling descriptors: %v", err)
	}
	files := protoregistry.NewNamespacedFiles(nil)
	types := protoregistry.NewNamespacedTypes(nil)
	var rangeErr error
	compiled.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if err := files.RegisterFile(fd); err != nil {
			rangeErr = err
			return false
		}
		if err := registerFileTypes(types, fd); err != nil {
			rangeErr = err
			return false
		}
		return true
	})
	if rangeErr != nil {
		t.Fatalf("registering descriptors: %v", rangeErr)
	}
	return &schemaSnapshot{
		schemaID: schemaID,
		version:  1,
		files:    files,
		types:    types,
	}
}
