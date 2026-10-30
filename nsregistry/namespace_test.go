// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// SPDX-License-Identifier: BSD-3-Clause

package nsregistry_test

import (
	"fmt"
	"sync"
	"testing"

	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/trendvidia/protoregistry/nsregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

func mustMakeFileNS(s string, deps ...protoreflect.FileDescriptor) protoreflect.FileDescriptor {
	pb := new(descriptorpb.FileDescriptorProto)
	if err := prototext.Unmarshal([]byte(s), pb); err != nil {
		panic(err)
	}
	var resolver protodesc.Resolver
	if len(deps) > 0 {
		var files protoregistry.Files
		for _, d := range deps {
			if err := files.RegisterFile(d); err != nil {
				panic(err)
			}
		}
		resolver = &files
	}
	fd, err := protodesc.NewFile(pb, resolver)
	if err != nil {
		panic(err)
	}
	return fd
}

// fileWithMessage creates a file descriptor containing a single message.
func fileWithMessage(filePath, pkg, msgName string) protoreflect.FileDescriptor {
	return mustMakeFileNS(`
		syntax: "proto2"
		name: "` + filePath + `"
		package: "` + pkg + `"
		message_type: [{ name: "` + msgName + `" }]
	`)
}

// fileWithEnum creates a file descriptor containing a single enum.
func fileWithEnum(filePath, pkg, enumName string) protoreflect.FileDescriptor {
	return mustMakeFileNS(`
		syntax: "proto2"
		name: "` + filePath + `"
		package: "` + pkg + `"
		enum_type: [{
			name: "` + enumName + `"
			value: [{ name: "UNKNOWN" number: 0 }]
		}]
	`)
}

// fileWithTwoMessages creates a file descriptor with two messages.
func fileWithTwoMessages(filePath, pkg, msg1, msg2 string) protoreflect.FileDescriptor {
	return mustMakeFileNS(`
		syntax: "proto2"
		name: "` + filePath + `"
		package: "` + pkg + `"
		message_type: [{ name: "` + msg1 + `" }, { name: "` + msg2 + `" }]
	`)
}

func TestNamespacedFiles_RegisterAndFind(t *testing.T) {
	ns := nsregistry.NewNamespacedFiles(nil)
	fd := fileWithMessage("test.proto", "pkg", "Foo")

	if err := ns.RegisterFile(fd); err != nil {
		t.Fatalf("RegisterFile: %v", err)
	}

	// Find by path.
	got, err := ns.FindFileByPath("test.proto")
	if err != nil {
		t.Fatalf("FindFileByPath: %v", err)
	}
	if got.Path() != "test.proto" {
		t.Errorf("got path %q, want test.proto", got.Path())
	}

	// Find descriptor by name.
	d, err := ns.FindDescriptorByName("pkg.Foo")
	if err != nil {
		t.Fatalf("FindDescriptorByName: %v", err)
	}
	if d.FullName() != "pkg.Foo" {
		t.Errorf("got %v, want pkg.Foo", d.FullName())
	}

	// NumFiles.
	if n := ns.NumFiles(); n != 1 {
		t.Errorf("NumFiles = %d, want 1", n)
	}
}

func TestNamespacedFiles_DuplicateRegisterFails(t *testing.T) {
	ns := nsregistry.NewNamespacedFiles(nil)
	fd := fileWithMessage("test.proto", "pkg", "Foo")

	if err := ns.RegisterFile(fd); err != nil {
		t.Fatalf("RegisterFile: %v", err)
	}
	if err := ns.RegisterFile(fd); err == nil {
		t.Fatal("expected error on duplicate RegisterFile, got nil")
	}
}

func TestNamespacedFiles_ParentFallback(t *testing.T) {
	parent := nsregistry.NewNamespacedFiles(nil)
	parentFile := fileWithMessage("parent.proto", "pkg", "ParentMsg")
	if err := parent.RegisterFile(parentFile); err != nil {
		t.Fatalf("parent RegisterFile: %v", err)
	}

	child := nsregistry.NewNamespacedFiles(parent)
	childFile := fileWithMessage("child.proto", "pkg", "ChildMsg")
	if err := child.RegisterFile(childFile); err != nil {
		t.Fatalf("child RegisterFile: %v", err)
	}

	// Child can find its own descriptor.
	if _, err := child.FindDescriptorByName("pkg.ChildMsg"); err != nil {
		t.Fatalf("child FindDescriptorByName(ChildMsg): %v", err)
	}

	// Child falls back to parent.
	if _, err := child.FindDescriptorByName("pkg.ParentMsg"); err != nil {
		t.Fatalf("child FindDescriptorByName(ParentMsg): %v", err)
	}

	// Parent does NOT see child.
	if _, err := parent.FindDescriptorByName("pkg.ChildMsg"); err != protoregistry.NotFound {
		t.Fatalf("parent FindDescriptorByName(ChildMsg): got %v, want NotFound", err)
	}

	// FindFileByPath falls back.
	if _, err := child.FindFileByPath("parent.proto"); err != nil {
		t.Fatalf("child FindFileByPath(parent.proto): %v", err)
	}
}

func TestNamespacedFiles_ChildShadowsParent(t *testing.T) {
	parent := nsregistry.NewNamespacedFiles(nil)
	parentFile := fileWithMessage("shared.proto", "pkg", "Msg")
	if err := parent.RegisterFile(parentFile); err != nil {
		t.Fatalf("parent RegisterFile: %v", err)
	}

	child := nsregistry.NewNamespacedFiles(parent)
	childFile := fileWithTwoMessages("shared.proto", "pkg", "Msg", "Extra")
	if err := child.RegisterFile(childFile); err != nil {
		t.Fatalf("child RegisterFile: %v", err)
	}

	// Child sees its own version (the one with Extra).
	if _, err := child.FindDescriptorByName("pkg.Extra"); err != nil {
		t.Fatalf("child FindDescriptorByName(Extra): %v", err)
	}

	// Parent does NOT see Extra.
	if _, err := parent.FindDescriptorByName("pkg.Extra"); err != protoregistry.NotFound {
		t.Fatalf("parent FindDescriptorByName(Extra): got %v, want NotFound", err)
	}

	// File lookup: child returns its version.
	fd, err := child.FindFileByPath("shared.proto")
	if err != nil {
		t.Fatalf("child FindFileByPath: %v", err)
	}
	if fd.Messages().Len() != 2 {
		t.Errorf("child file has %d messages, want 2", fd.Messages().Len())
	}
}

func TestNamespacedFiles_UpdateFile_FixesFilesByPath(t *testing.T) {
	ns := nsregistry.NewNamespacedFiles(nil)

	v1 := fileWithMessage("svc.proto", "pkg", "MsgV1")
	if err := ns.RegisterFile(v1); err != nil {
		t.Fatalf("RegisterFile v1: %v", err)
	}

	v2 := fileWithMessage("svc.proto", "pkg", "MsgV2")
	if err := ns.UpdateFile(v2); err != nil {
		t.Fatalf("UpdateFile v2: %v", err)
	}

	// FindFileByPath must return v2, not stale v1.
	fd, err := ns.FindFileByPath("svc.proto")
	if err != nil {
		t.Fatalf("FindFileByPath after update: %v", err)
	}
	if fd.Messages().Len() != 1 || fd.Messages().Get(0).Name() != "MsgV2" {
		t.Errorf("FindFileByPath returned stale file, got message %v", fd.Messages().Get(0).Name())
	}
}

func TestNamespacedFiles_UpdateFile_CleansStaleDescriptors(t *testing.T) {
	ns := nsregistry.NewNamespacedFiles(nil)

	v1 := fileWithTwoMessages("svc.proto", "pkg", "Keep", "Remove")
	if err := ns.RegisterFile(v1); err != nil {
		t.Fatalf("RegisterFile v1: %v", err)
	}

	// Verify both exist.
	if _, err := ns.FindDescriptorByName("pkg.Keep"); err != nil {
		t.Fatalf("FindDescriptorByName(Keep) before update: %v", err)
	}
	if _, err := ns.FindDescriptorByName("pkg.Remove"); err != nil {
		t.Fatalf("FindDescriptorByName(Remove) before update: %v", err)
	}

	// Update: v2 drops "Remove".
	v2 := fileWithMessage("svc.proto", "pkg", "Keep")
	if err := ns.UpdateFile(v2); err != nil {
		t.Fatalf("UpdateFile v2: %v", err)
	}

	// "Keep" should still be findable.
	if _, err := ns.FindDescriptorByName("pkg.Keep"); err != nil {
		t.Fatalf("FindDescriptorByName(Keep) after update: %v", err)
	}

	// "Remove" must be gone.
	if _, err := ns.FindDescriptorByName("pkg.Remove"); err != protoregistry.NotFound {
		t.Fatalf("FindDescriptorByName(Remove) after update: got %v, want NotFound", err)
	}

	// NumFiles must still be 1.
	if n := ns.NumFiles(); n != 1 {
		t.Errorf("NumFiles = %d, want 1", n)
	}
}

func TestNamespacedFiles_UpdateFile_UpsertNew(t *testing.T) {
	ns := nsregistry.NewNamespacedFiles(nil)

	// Update on empty namespace should work like register.
	fd := fileWithMessage("new.proto", "pkg", "Msg")
	if err := ns.UpdateFile(fd); err != nil {
		t.Fatalf("UpdateFile on empty: %v", err)
	}
	if _, err := ns.FindDescriptorByName("pkg.Msg"); err != nil {
		t.Fatalf("FindDescriptorByName after upsert: %v", err)
	}
	if n := ns.NumFiles(); n != 1 {
		t.Errorf("NumFiles = %d, want 1", n)
	}
}

func TestNamespacedFiles_UpdateFile_PackageChange(t *testing.T) {
	ns := nsregistry.NewNamespacedFiles(nil)

	v1 := fileWithMessage("svc.proto", "old.pkg", "Msg")
	if err := ns.RegisterFile(v1); err != nil {
		t.Fatalf("RegisterFile v1: %v", err)
	}
	if _, err := ns.FindDescriptorByName("old.pkg.Msg"); err != nil {
		t.Fatalf("FindDescriptorByName(old.pkg.Msg): %v", err)
	}

	v2 := fileWithMessage("svc.proto", "new.pkg", "Msg")
	if err := ns.UpdateFile(v2); err != nil {
		t.Fatalf("UpdateFile v2: %v", err)
	}

	// Old package descriptor gone.
	if _, err := ns.FindDescriptorByName("old.pkg.Msg"); err != protoregistry.NotFound {
		t.Fatalf("FindDescriptorByName(old.pkg.Msg): got %v, want NotFound", err)
	}
	// New package works.
	if _, err := ns.FindDescriptorByName("new.pkg.Msg"); err != nil {
		t.Fatalf("FindDescriptorByName(new.pkg.Msg): %v", err)
	}
}

func TestNamespacedFiles_Unregister(t *testing.T) {
	ns := nsregistry.NewNamespacedFiles(nil)

	fd := fileWithMessage("rm.proto", "pkg", "Gone")
	if err := ns.RegisterFile(fd); err != nil {
		t.Fatalf("RegisterFile: %v", err)
	}

	if err := ns.UnregisterFile("rm.proto"); err != nil {
		t.Fatalf("UnregisterFile: %v", err)
	}

	if _, err := ns.FindFileByPath("rm.proto"); err != protoregistry.NotFound {
		t.Fatalf("FindFileByPath after unregister: got %v, want NotFound", err)
	}
	if _, err := ns.FindDescriptorByName("pkg.Gone"); err != protoregistry.NotFound {
		t.Fatalf("FindDescriptorByName after unregister: got %v, want NotFound", err)
	}
	if n := ns.NumFiles(); n != 0 {
		t.Errorf("NumFiles = %d, want 0", n)
	}
}

func TestNamespacedFiles_Unregister_FallsBackToParent(t *testing.T) {
	parent := nsregistry.NewNamespacedFiles(nil)
	parentFile := fileWithMessage("shared.proto", "pkg", "Base")
	if err := parent.RegisterFile(parentFile); err != nil {
		t.Fatalf("parent RegisterFile: %v", err)
	}

	child := nsregistry.NewNamespacedFiles(parent)
	childOverride := fileWithMessage("shared.proto", "pkg", "Override")
	if err := child.RegisterFile(childOverride); err != nil {
		t.Fatalf("child RegisterFile: %v", err)
	}

	// Child sees Override.
	d, _ := child.FindDescriptorByName("pkg.Override")
	if d == nil {
		t.Fatal("expected Override in child")
	}

	// Unregister from child: now fallback to parent.
	if err := child.UnregisterFile("shared.proto"); err != nil {
		t.Fatalf("UnregisterFile: %v", err)
	}

	// Child falls back to parent's version.
	d, err := child.FindDescriptorByName("pkg.Base")
	if err != nil {
		t.Fatalf("FindDescriptorByName(Base) after unregister: %v", err)
	}
	if d.FullName() != "pkg.Base" {
		t.Errorf("got %v, want pkg.Base", d.FullName())
	}

	// Override is gone.
	if _, err := child.FindDescriptorByName("pkg.Override"); err != protoregistry.NotFound {
		t.Fatalf("FindDescriptorByName(Override) after unregister: got %v, want NotFound", err)
	}
}

func TestNamespacedFiles_UnregisterNotFound(t *testing.T) {
	ns := nsregistry.NewNamespacedFiles(nil)
	if err := ns.UnregisterFile("nonexistent.proto"); err != protoregistry.NotFound {
		t.Fatalf("UnregisterFile: got %v, want NotFound", err)
	}
}

func TestNamespacedFiles_RangeFilesAll(t *testing.T) {
	parent := nsregistry.NewNamespacedFiles(nil)
	if err := parent.RegisterFile(fileWithMessage("a.proto", "pkg", "A")); err != nil {
		t.Fatal(err)
	}
	if err := parent.RegisterFile(fileWithMessage("b.proto", "pkg", "B")); err != nil {
		t.Fatal(err)
	}

	child := nsregistry.NewNamespacedFiles(parent)
	// Shadow b.proto with a different version.
	if err := child.RegisterFile(fileWithMessage("b.proto", "pkg", "B2")); err != nil {
		t.Fatal(err)
	}
	if err := child.RegisterFile(fileWithMessage("c.proto", "pkg", "C")); err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]bool)
	child.RangeFilesAll(func(fd protoreflect.FileDescriptor) bool {
		seen[fd.Path()] = true
		// b.proto should come from child (B2), not parent.
		if fd.Path() == "b.proto" {
			if fd.Messages().Get(0).Name() != "B2" {
				t.Errorf("b.proto should be child's version (B2), got %v", fd.Messages().Get(0).Name())
			}
		}
		return true
	})

	if !seen["a.proto"] || !seen["b.proto"] || !seen["c.proto"] {
		t.Errorf("expected a.proto, b.proto, c.proto; got %v", seen)
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 files, got %d: %v", len(seen), seen)
	}
}

// --- NamespacedTypes tests ---

func TestNamespacedTypes_RegisterAndFind(t *testing.T) {
	ns := nsregistry.NewNamespacedTypes(nil)
	fd := fileWithMessage("test.proto", "pkg", "Foo")
	mt := dynamicpb.NewMessageType(fd.Messages().Get(0))

	if err := ns.RegisterMessage(mt); err != nil {
		t.Fatalf("RegisterMessage: %v", err)
	}

	got, err := ns.FindMessageByName("pkg.Foo")
	if err != nil {
		t.Fatalf("FindMessageByName: %v", err)
	}
	if got.Descriptor().FullName() != "pkg.Foo" {
		t.Errorf("got %v, want pkg.Foo", got.Descriptor().FullName())
	}

	if n := ns.NumMessages(); n != 1 {
		t.Errorf("NumMessages = %d, want 1", n)
	}
}

func TestNamespacedTypes_ParentFallback(t *testing.T) {
	parent := nsregistry.NewNamespacedTypes(nil)
	parentFD := fileWithMessage("p.proto", "pkg", "ParentMsg")
	if err := parent.RegisterMessage(dynamicpb.NewMessageType(parentFD.Messages().Get(0))); err != nil {
		t.Fatal(err)
	}

	child := nsregistry.NewNamespacedTypes(parent)
	childFD := fileWithMessage("c.proto", "pkg", "ChildMsg")
	if err := child.RegisterMessage(dynamicpb.NewMessageType(childFD.Messages().Get(0))); err != nil {
		t.Fatal(err)
	}

	// Child finds its own.
	if _, err := child.FindMessageByName("pkg.ChildMsg"); err != nil {
		t.Fatalf("child FindMessageByName(ChildMsg): %v", err)
	}
	// Child falls back to parent.
	if _, err := child.FindMessageByName("pkg.ParentMsg"); err != nil {
		t.Fatalf("child FindMessageByName(ParentMsg): %v", err)
	}
	// Parent does not see child.
	if _, err := parent.FindMessageByName("pkg.ChildMsg"); err != protoregistry.NotFound {
		t.Fatalf("parent FindMessageByName(ChildMsg): got %v, want NotFound", err)
	}
}

func TestNamespacedTypes_UpdateMessage(t *testing.T) {
	ns := nsregistry.NewNamespacedTypes(nil)
	fd1 := fileWithMessage("v1.proto", "pkg", "Msg")
	fd2 := fileWithMessage("v2.proto", "pkg", "Msg")
	mt1 := dynamicpb.NewMessageType(fd1.Messages().Get(0))
	mt2 := dynamicpb.NewMessageType(fd2.Messages().Get(0))

	if err := ns.RegisterMessage(mt1); err != nil {
		t.Fatal(err)
	}
	if n := ns.NumMessages(); n != 1 {
		t.Fatalf("NumMessages = %d, want 1", n)
	}

	// Update replaces without error.
	if err := ns.UpdateMessage(mt2); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}
	// Count stays at 1.
	if n := ns.NumMessages(); n != 1 {
		t.Fatalf("NumMessages after update = %d, want 1", n)
	}
}

func TestNamespacedTypes_UpdateMessage_Upsert(t *testing.T) {
	ns := nsregistry.NewNamespacedTypes(nil)
	fd := fileWithMessage("test.proto", "pkg", "Msg")
	mt := dynamicpb.NewMessageType(fd.Messages().Get(0))

	// Update on empty works like register.
	if err := ns.UpdateMessage(mt); err != nil {
		t.Fatalf("UpdateMessage on empty: %v", err)
	}
	if n := ns.NumMessages(); n != 1 {
		t.Errorf("NumMessages = %d, want 1", n)
	}
}

func TestNamespacedTypes_UnregisterMessage(t *testing.T) {
	ns := nsregistry.NewNamespacedTypes(nil)
	fd := fileWithMessage("test.proto", "pkg", "Msg")
	if err := ns.RegisterMessage(dynamicpb.NewMessageType(fd.Messages().Get(0))); err != nil {
		t.Fatal(err)
	}
	if err := ns.UnregisterMessage("pkg.Msg"); err != nil {
		t.Fatalf("UnregisterMessage: %v", err)
	}
	if _, err := ns.FindMessageByName("pkg.Msg"); err != protoregistry.NotFound {
		t.Fatalf("FindMessageByName after unregister: got %v, want NotFound", err)
	}
	if n := ns.NumMessages(); n != 0 {
		t.Errorf("NumMessages = %d, want 0", n)
	}
}

func TestNamespacedTypes_Enum(t *testing.T) {
	ns := nsregistry.NewNamespacedTypes(nil)
	fd := fileWithEnum("test.proto", "pkg", "Color")
	et := dynamicpb.NewEnumType(fd.Enums().Get(0))

	if err := ns.RegisterEnum(et); err != nil {
		t.Fatalf("RegisterEnum: %v", err)
	}
	got, err := ns.FindEnumByName("pkg.Color")
	if err != nil {
		t.Fatalf("FindEnumByName: %v", err)
	}
	if got.Descriptor().FullName() != "pkg.Color" {
		t.Errorf("got %v, want pkg.Color", got.Descriptor().FullName())
	}
	if n := ns.NumEnums(); n != 1 {
		t.Errorf("NumEnums = %d, want 1", n)
	}

	// Update.
	if err := ns.UpdateEnum(et); err != nil {
		t.Fatalf("UpdateEnum: %v", err)
	}
	if n := ns.NumEnums(); n != 1 {
		t.Errorf("NumEnums after update = %d, want 1", n)
	}

	// Unregister.
	if err := ns.UnregisterEnum("pkg.Color"); err != nil {
		t.Fatalf("UnregisterEnum: %v", err)
	}
	if _, err := ns.FindEnumByName("pkg.Color"); err != protoregistry.NotFound {
		t.Fatalf("after unregister: got %v, want NotFound", err)
	}
}

func TestNamespacedTypes_FindMessageByURL(t *testing.T) {
	ns := nsregistry.NewNamespacedTypes(nil)
	fd := fileWithMessage("test.proto", "pkg", "Msg")
	if err := ns.RegisterMessage(dynamicpb.NewMessageType(fd.Messages().Get(0))); err != nil {
		t.Fatal(err)
	}
	got, err := ns.FindMessageByURL("type.googleapis.com/pkg.Msg")
	if err != nil {
		t.Fatalf("FindMessageByURL: %v", err)
	}
	if got.Descriptor().FullName() != "pkg.Msg" {
		t.Errorf("got %v, want pkg.Msg", got.Descriptor().FullName())
	}
}

func TestNamespacedTypes_FindMessageByURL_Fallback(t *testing.T) {
	parent := nsregistry.NewNamespacedTypes(nil)
	fd := fileWithMessage("test.proto", "pkg", "Msg")
	if err := parent.RegisterMessage(dynamicpb.NewMessageType(fd.Messages().Get(0))); err != nil {
		t.Fatal(err)
	}

	child := nsregistry.NewNamespacedTypes(parent)
	got, err := child.FindMessageByURL("type.googleapis.com/pkg.Msg")
	if err != nil {
		t.Fatalf("FindMessageByURL fallback: %v", err)
	}
	if got.Descriptor().FullName() != "pkg.Msg" {
		t.Errorf("got %v, want pkg.Msg", got.Descriptor().FullName())
	}
}

// --- Namespace (aggregate) tests ---

func TestNamespace_ThreeLevelHierarchy(t *testing.T) {
	// global -> shared -> tenant
	shared := nsregistry.NewRootNamespace()
	sharedFile := fileWithMessage("shared.proto", "common", "Base")
	if err := shared.Files.RegisterFile(sharedFile); err != nil {
		t.Fatal(err)
	}
	if err := shared.Types.RegisterMessage(dynamicpb.NewMessageType(sharedFile.Messages().Get(0))); err != nil {
		t.Fatal(err)
	}

	tenantA := nsregistry.NewNamespace(shared)
	tenantAFile := fileWithMessage("tenant_a.proto", "tenant", "Config")
	if err := tenantA.Files.RegisterFile(tenantAFile); err != nil {
		t.Fatal(err)
	}
	if err := tenantA.Types.RegisterMessage(dynamicpb.NewMessageType(tenantAFile.Messages().Get(0))); err != nil {
		t.Fatal(err)
	}

	tenantB := nsregistry.NewNamespace(shared)
	tenantBFile := fileWithMessage("tenant_b.proto", "tenant", "Settings")
	if err := tenantB.Files.RegisterFile(tenantBFile); err != nil {
		t.Fatal(err)
	}

	// Tenant A sees shared + own.
	if _, err := tenantA.Files.FindDescriptorByName("common.Base"); err != nil {
		t.Fatalf("tenantA shared lookup: %v", err)
	}
	if _, err := tenantA.Files.FindDescriptorByName("tenant.Config"); err != nil {
		t.Fatalf("tenantA own lookup: %v", err)
	}
	if _, err := tenantA.Types.FindMessageByName("common.Base"); err != nil {
		t.Fatalf("tenantA type fallback: %v", err)
	}

	// Tenant A does NOT see tenant B.
	if _, err := tenantA.Files.FindDescriptorByName("tenant.Settings"); err != protoregistry.NotFound {
		t.Fatalf("tenantA should not see tenantB: got %v", err)
	}

	// Tenant B sees shared but not tenant A.
	if _, err := tenantB.Files.FindDescriptorByName("common.Base"); err != nil {
		t.Fatalf("tenantB shared lookup: %v", err)
	}
	if _, err := tenantB.Files.FindDescriptorByName("tenant.Config"); err != protoregistry.NotFound {
		t.Fatalf("tenantB should not see tenantA: got %v", err)
	}
}

func TestNamespaceOverGlobal_ReadOnly(t *testing.T) {
	ns := nsregistry.NewNamespaceOverGlobal()

	// Writing to the global wrapper must fail.
	fd := fileWithMessage("nope.proto", "pkg", "Nope")
	// The parent (global wrapper) is read-only, but the child is writable.
	// Verify the child works.
	if err := ns.Files.RegisterFile(fd); err != nil {
		t.Fatalf("RegisterFile on child over global: %v", err)
	}
	if _, err := ns.Files.FindFileByPath("nope.proto"); err != nil {
		t.Fatalf("FindFileByPath: %v", err)
	}
}

// --- Concurrency tests ---

func TestNamespacedFiles_ConcurrentSiblings(t *testing.T) {
	parent := nsregistry.NewNamespacedFiles(nil)
	parentFile := fileWithMessage("base.proto", "base", "Common")
	if err := parent.RegisterFile(parentFile); err != nil {
		t.Fatal(err)
	}

	const numSiblings = 8
	var wg sync.WaitGroup
	errs := make(chan error, numSiblings*3)

	for i := 0; i < numSiblings; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sibling := nsregistry.NewNamespacedFiles(parent)

			// Register.
			fd := fileWithMessage(
				"sib.proto",
				"sib",
				"Msg",
			)
			if err := sibling.RegisterFile(fd); err != nil {
				errs <- err
				return
			}

			// Read own.
			if _, err := sibling.FindDescriptorByName("sib.Msg"); err != nil {
				errs <- err
				return
			}

			// Read parent.
			if _, err := sibling.FindDescriptorByName("base.Common"); err != nil {
				errs <- err
				return
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent error: %v", err)
	}
}

func TestNamespacedTypes_ConcurrentReadWrite(t *testing.T) {
	ns := nsregistry.NewNamespacedTypes(nil)

	// Pre-register a message.
	fd := fileWithMessage("base.proto", "pkg", "Base")
	if err := ns.RegisterMessage(dynamicpb.NewMessageType(fd.Messages().Get(0))); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 100)

	// Concurrent readers.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if _, err := ns.FindMessageByName("pkg.Base"); err != nil {
					errs <- err
					return
				}
			}
		}()
	}

	// Concurrent updater.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			mt := dynamicpb.NewMessageType(fd.Messages().Get(0))
			if err := ns.UpdateMessage(mt); err != nil {
				errs <- err
				return
			}
		}
	}()

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent error: %v", err)
	}
}

func TestNamespacedFiles_NameConflict(t *testing.T) {
	ns := nsregistry.NewNamespacedFiles(nil)

	fd1 := fileWithMessage("a.proto", "pkg", "Msg")
	fd2 := fileWithMessage("b.proto", "pkg", "Msg")

	if err := ns.RegisterFile(fd1); err != nil {
		t.Fatal(err)
	}
	// Second file defines same top-level name: should fail.
	if err := ns.RegisterFile(fd2); err == nil {
		t.Fatal("expected conflict error, got nil")
	}
}

func TestNamespacedTypes_DuplicateRegisterFails(t *testing.T) {
	ns := nsregistry.NewNamespacedTypes(nil)
	fd := fileWithMessage("test.proto", "pkg", "Msg")
	mt := dynamicpb.NewMessageType(fd.Messages().Get(0))

	if err := ns.RegisterMessage(mt); err != nil {
		t.Fatal(err)
	}
	if err := ns.RegisterMessage(mt); err == nil {
		t.Fatal("expected duplicate error, got nil")
	}
}

func TestNamespacedFiles_RangeFiles(t *testing.T) {
	ns := nsregistry.NewNamespacedFiles(nil)
	if err := ns.RegisterFile(fileWithMessage("a.proto", "pkg", "A")); err != nil {
		t.Fatal(err)
	}
	if err := ns.RegisterFile(fileWithMessage("b.proto", "pkg", "B")); err != nil {
		t.Fatal(err)
	}

	seen := make(map[string]bool)
	ns.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		seen[fd.Path()] = true
		return true
	})
	if len(seen) != 2 || !seen["a.proto"] || !seen["b.proto"] {
		t.Errorf("RangeFiles: got %v", seen)
	}
}

func TestNamespacedFiles_NumFilesByPackage(t *testing.T) {
	ns := nsregistry.NewNamespacedFiles(nil)
	if err := ns.RegisterFile(fileWithMessage("a.proto", "mypkg", "A")); err != nil {
		t.Fatal(err)
	}
	if err := ns.RegisterFile(fileWithMessage("b.proto", "mypkg", "B")); err != nil {
		t.Fatal(err)
	}
	if err := ns.RegisterFile(fileWithMessage("c.proto", "other", "C")); err != nil {
		t.Fatal(err)
	}

	if n := ns.NumFilesByPackage("mypkg"); n != 2 {
		t.Errorf("NumFilesByPackage(mypkg) = %d, want 2", n)
	}
	if n := ns.NumFilesByPackage("other"); n != 1 {
		t.Errorf("NumFilesByPackage(other) = %d, want 1", n)
	}
	if n := ns.NumFilesByPackage("nonexistent"); n != 0 {
		t.Errorf("NumFilesByPackage(nonexistent) = %d, want 0", n)
	}
}

func TestNamespacedTypes_EnumFallback(t *testing.T) {
	parent := nsregistry.NewNamespacedTypes(nil)
	fd := fileWithEnum("test.proto", "pkg", "Status")
	if err := parent.RegisterEnum(dynamicpb.NewEnumType(fd.Enums().Get(0))); err != nil {
		t.Fatal(err)
	}

	child := nsregistry.NewNamespacedTypes(parent)
	got, err := child.FindEnumByName("pkg.Status")
	if err != nil {
		t.Fatalf("child FindEnumByName: %v", err)
	}
	if got.Descriptor().FullName() != "pkg.Status" {
		t.Errorf("got %v, want pkg.Status", got.Descriptor().FullName())
	}
}

func TestNamespacedTypes_UnregisterNotFound(t *testing.T) {
	ns := nsregistry.NewNamespacedTypes(nil)
	if err := ns.UnregisterMessage("nonexistent.Msg"); err != protoregistry.NotFound {
		t.Fatalf("got %v, want NotFound", err)
	}
	if err := ns.UnregisterEnum("nonexistent.Enum"); err != protoregistry.NotFound {
		t.Fatalf("got %v, want NotFound", err)
	}
	if err := ns.UnregisterExtension("nonexistent.Ext"); err != protoregistry.NotFound {
		t.Fatalf("got %v, want NotFound", err)
	}
}

// A namespace over the globals takes no global lock of its own: the globals'
// methods lock for themselves. Lookups through it must stay race-free while
// something registers into GlobalFiles, which is what generated code does in
// init and a lazily loaded plugin can do later.
func TestNamespaceOverGlobal_ConcurrentGlobalRegistration(t *testing.T) {
	ns := nsregistry.NewNamespaceOverGlobal()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range n {
			fd := fileWithMessage(fmt.Sprintf("nsregistry/race%d.proto", i), fmt.Sprintf("nsregistry.race%d", i), "M")
			if err := protoregistry.GlobalFiles.RegisterFile(fd); err != nil {
				t.Errorf("RegisterFile: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := range n {
			path := fmt.Sprintf("nsregistry/race%d.proto", i)
			if _, err := ns.Files.FindFileByPath(path); err != nil && err != protoregistry.NotFound {
				t.Errorf("FindFileByPath(%q): %v", path, err)
			}
			ns.Files.NumFiles()
		}
	}()
	wg.Wait()

	if _, err := ns.Files.FindFileByPath("nsregistry/race0.proto"); err != nil {
		t.Errorf("registered file not visible through the namespace: %v", err)
	}
}
