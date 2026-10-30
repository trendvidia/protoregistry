// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// SPDX-License-Identifier: BSD-3-Clause

// Package nsregistry provides file and type registries with per-namespace
// isolation and hierarchical fallback, for a process that serves schemas which
// would conflict in the global registries of
// [google.golang.org/protobuf/reflect/protoregistry].
//
// It came from the trendvidia/protobuf-go fork and needs only protobuf's public
// API. The code is BSD-licensed; see the LICENSE file in this directory.
package nsregistry

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// Namespace bundles a NamespacedFiles and NamespacedTypes together, providing
// an isolated, hierarchical registry for multi-tenant or dynamic-plugin use.
// A child namespace falls back to its parent for lookups, but mutations only
// affect the local namespace. Globals are treated as read-only.
type Namespace struct {
	Files *NamespacedFiles
	Types *NamespacedTypes
}

// NewNamespace creates a child namespace whose lookups fall back to parent.
// Pass nil to create a root namespace with no fallback.
func NewNamespace(parent *Namespace) *Namespace {
	var pf *NamespacedFiles
	var pt *NamespacedTypes
	if parent != nil {
		pf = parent.Files
		pt = parent.Types
	}
	return &Namespace{
		Files: NewNamespacedFiles(pf),
		Types: NewNamespacedTypes(pt),
	}
}

// NewRootNamespace creates a root namespace with no parent fallback.
func NewRootNamespace() *Namespace {
	return NewNamespace(nil)
}

// NewNamespaceOverGlobal creates a namespace that falls back to protoregistry.GlobalFiles
// and protoregistry.GlobalTypes for lookups. The globals are read-only through this handle.
func NewNamespaceOverGlobal() *Namespace {
	return &Namespace{
		Files: NewNamespacedFiles(&NamespacedFiles{global: protoregistry.GlobalFiles}),
		Types: NewNamespacedTypes(&NamespacedTypes{global: protoregistry.GlobalTypes}),
	}
}

// NamespacedFiles is a file descriptor registry with hierarchical fallback.
// Mutations (Register, Update, Unregister) affect only the local registry.
// Lookups check the local registry first, then walk the parent chain.
//
// Safe for concurrent use.
type NamespacedFiles struct {
	mu     sync.RWMutex
	parent *NamespacedFiles

	// global is non-nil only for the read-only wrapper around protoregistry.GlobalFiles.
	// When set, all write methods return an error and reads delegate to protoregistry.GlobalFiles.
	global *protoregistry.Files

	descsByName map[protoreflect.FullName]any
	filesByPath map[string][]protoreflect.FileDescriptor
	numFiles    int

	// fileDescs tracks which descriptor full names each file contributed,
	// keyed by file path. This enables proper cleanup on Update/Unregister.
	fileDescs map[string]fileRecord
}

// fileRecord tracks metadata about a registered file needed for cleanup.
type fileRecord struct {
	pkg   protoreflect.FullName   // package of the file at registration time
	names []protoreflect.FullName // top-level descriptor names contributed
}

// NewNamespacedFiles creates a new NamespacedFiles whose lookups fall back to parent.
// Pass nil to create a root namespace with no fallback.
func NewNamespacedFiles(parent *NamespacedFiles) *NamespacedFiles {
	return &NamespacedFiles{
		parent:      parent,
		descsByName: map[protoreflect.FullName]any{"": &packageDescriptor{}},
		filesByPath: make(map[string][]protoreflect.FileDescriptor),
		fileDescs:   make(map[string]fileRecord),
	}
}

func (r *NamespacedFiles) isGlobalWrapper() bool {
	return r.global != nil
}

// RegisterFile registers the provided file descriptor in the local namespace.
// Returns an error if a descriptor within the file conflicts with an existing
// local registration.
func (r *NamespacedFiles) RegisterFile(file protoreflect.FileDescriptor) error {
	if r.isGlobalWrapper() {
		return errors.New("nsregistry: cannot register files on read-only global namespace")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	path := file.Path()
	if prev := r.filesByPath[path]; len(prev) > 0 {
		return fmt.Errorf("nsregistry: file %q is already registered", path)
	}

	// Check for package-name conflicts against local descriptors.
	for name := file.Package(); name != ""; name = name.Parent() {
		switch r.descsByName[name].(type) {
		case nil, *packageDescriptor:
		default:
			return fmt.Errorf("nsregistry: file %q has a package name conflict over %v", file.Path(), name)
		}
	}

	// Check for top-level descriptor name conflicts.
	var conflictErr error
	rangeTopLevelDescriptors(file, func(d protoreflect.Descriptor) {
		if conflictErr != nil {
			return
		}
		if prev := r.descsByName[d.FullName()]; prev != nil {
			conflictErr = fmt.Errorf("nsregistry: file %q has a name conflict over %v", file.Path(), d.FullName())
		}
	})
	if conflictErr != nil {
		return conflictErr
	}

	r.insertFileLocked(file)
	return nil
}

// UpdateFile registers or replaces the file at the given path.
// If a file with the same path already exists, stale descriptors are removed
// and all indexes are updated. If no file with that path exists, this behaves
// like RegisterFile but without conflict checks (upsert semantics).
func (r *NamespacedFiles) UpdateFile(file protoreflect.FileDescriptor) error {
	if r.isGlobalWrapper() {
		return errors.New("nsregistry: cannot update files on read-only global namespace")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	path := file.Path()
	if old, ok := r.fileDescs[path]; ok {
		// Compute the set of names the new file provides.
		newNames := make(map[protoreflect.FullName]struct{})
		rangeTopLevelDescriptors(file, func(d protoreflect.Descriptor) {
			newNames[d.FullName()] = struct{}{}
		})

		// Remove stale descriptors: names the old file had that the new one doesn't.
		for _, name := range old.names {
			if _, keep := newNames[name]; !keep {
				delete(r.descsByName, name)
			}
		}

		// If the package changed, remove the file from the old package descriptor.
		if old.pkg != file.Package() {
			if p, ok := r.descsByName[old.pkg].(*packageDescriptor); ok {
				p.files = removeFileByPath(p.files, path)
				r.cleanupPackageChain(old.pkg)
			}
		}

		// Remove from filesByPath and fileDescs so insertFileLocked can re-add cleanly.
		delete(r.filesByPath, path)
		delete(r.fileDescs, path)

		// Remove from current package descriptor's files list (if same package).
		if old.pkg == file.Package() {
			if p, ok := r.descsByName[file.Package()].(*packageDescriptor); ok {
				p.files = removeFileByPath(p.files, path)
			}
		}

		r.numFiles--
	}

	r.insertFileLocked(file)
	return nil
}

// UnregisterFile removes the file at the given path and all its contributed
// descriptors from the local namespace. Returns protoregistry.NotFound if no file with that
// path is registered locally.
func (r *NamespacedFiles) UnregisterFile(path string) error {
	if r.isGlobalWrapper() {
		return errors.New("nsregistry: cannot unregister files on read-only global namespace")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	old, ok := r.fileDescs[path]
	if !ok {
		return protoregistry.NotFound
	}

	// Remove all descriptors contributed by this file.
	for _, name := range old.names {
		delete(r.descsByName, name)
	}

	// Remove from the package descriptor's files list.
	if p, ok := r.descsByName[old.pkg].(*packageDescriptor); ok {
		p.files = removeFileByPath(p.files, path)
		r.cleanupPackageChain(old.pkg)
	}

	delete(r.filesByPath, path)
	delete(r.fileDescs, path)
	r.numFiles--
	return nil
}

// insertFileLocked adds a file and its descriptors to the local maps.
// Caller must hold r.mu.
func (r *NamespacedFiles) insertFileLocked(file protoreflect.FileDescriptor) {
	path := file.Path()

	// Ensure package descriptors exist.
	for name := file.Package(); name != ""; name = name.Parent() {
		if r.descsByName[name] == nil {
			r.descsByName[name] = &packageDescriptor{}
		}
	}

	// Add to package descriptor.
	p := r.descsByName[file.Package()].(*packageDescriptor)
	p.files = append(p.files, file)

	// Register top-level descriptors and build reverse index.
	var names []protoreflect.FullName
	rangeTopLevelDescriptors(file, func(d protoreflect.Descriptor) {
		r.descsByName[d.FullName()] = d
		names = append(names, d.FullName())
	})

	r.filesByPath[path] = []protoreflect.FileDescriptor{file}
	r.fileDescs[path] = fileRecord{pkg: file.Package(), names: names}
	r.numFiles++
}

// cleanupPackageChain removes empty packageDescriptor entries walking from
// pkg up to the root. This prevents empty package nodes from lingering after
// the last file in a package is removed.
func (r *NamespacedFiles) cleanupPackageChain(pkg protoreflect.FullName) {
	for name := pkg; name != ""; name = name.Parent() {
		p, ok := r.descsByName[name].(*packageDescriptor)
		if !ok || len(p.files) > 0 {
			break
		}
		// Only delete if no sub-packages exist. A sub-package would be a
		// packageDescriptor with a name that has this as a prefix.
		// For simplicity, only delete leaf packages (those with no files).
		delete(r.descsByName, name)
	}
}

// FindDescriptorByName looks up a descriptor by full name.
// Checks the local namespace first, then walks the parent chain.
//
// This returns (nil, protoregistry.NotFound) if not found.
func (r *NamespacedFiles) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	if r == nil {
		return nil, protoregistry.NotFound
	}
	if r.isGlobalWrapper() {
		return r.global.FindDescriptorByName(name)
	}
	r.mu.RLock()
	d, err := r.findDescriptorByNameLocal(name)
	r.mu.RUnlock()
	if err != protoregistry.NotFound {
		return d, err
	}
	return r.parent.FindDescriptorByName(name)
}

// findDescriptorByNameLocal searches only the local descsByName map.
// Caller must hold at least r.mu.RLock().
func (r *NamespacedFiles) findDescriptorByNameLocal(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	prefix := name
	suffix := nameSuffix("")
	for prefix != "" {
		if d, ok := r.descsByName[prefix]; ok {
			switch d := d.(type) {
			case protoreflect.EnumDescriptor:
				if d.FullName() == name {
					return d, nil
				}
			case protoreflect.EnumValueDescriptor:
				if d.FullName() == name {
					return d, nil
				}
			case protoreflect.MessageDescriptor:
				if d.FullName() == name {
					return d, nil
				}
				if d := findDescriptorInMessage(d, suffix); d != nil && d.FullName() == name {
					return d, nil
				}
			case protoreflect.ExtensionDescriptor:
				if d.FullName() == name {
					return d, nil
				}
			case protoreflect.ServiceDescriptor:
				if d.FullName() == name {
					return d, nil
				}
				if d := d.Methods().ByName(suffix.Pop()); d != nil && d.FullName() == name {
					return d, nil
				}
			}
			return nil, protoregistry.NotFound
		}
		prefix = prefix.Parent()
		suffix = nameSuffix(name[len(prefix)+len("."):])
	}
	return nil, protoregistry.NotFound
}

// FindFileByPath looks up a file by path.
// Checks the local namespace first, then walks the parent chain.
//
// This returns (nil, protoregistry.NotFound) if not found.
func (r *NamespacedFiles) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	if r == nil {
		return nil, protoregistry.NotFound
	}
	if r.isGlobalWrapper() {
		return r.global.FindFileByPath(path)
	}
	r.mu.RLock()
	fds := r.filesByPath[path]
	r.mu.RUnlock()
	if len(fds) == 1 {
		return fds[0], nil
	}
	if len(fds) > 1 {
		return nil, fmt.Errorf("nsregistry: multiple files named %q", path)
	}
	return r.parent.FindFileByPath(path)
}

// NumFiles reports the number of files registered in the local namespace.
func (r *NamespacedFiles) NumFiles() int {
	if r == nil {
		return 0
	}
	if r.isGlobalWrapper() {
		return r.global.NumFiles()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.numFiles
}

// RangeFiles iterates over files in the local namespace while f returns true.
// Iteration order is undefined.
func (r *NamespacedFiles) RangeFiles(f func(protoreflect.FileDescriptor) bool) {
	if r == nil {
		return
	}
	if r.isGlobalWrapper() {
		r.global.RangeFiles(f)
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, files := range r.filesByPath {
		for _, file := range files {
			if !f(file) {
				return
			}
		}
	}
}

// RangeFilesAll iterates over all files visible to this namespace (local first,
// then parent chain). Files in a child namespace shadow files with the same path
// in a parent. Iteration order is undefined.
func (r *NamespacedFiles) RangeFilesAll(f func(protoreflect.FileDescriptor) bool) {
	if r == nil {
		return
	}
	seen := make(map[string]struct{})
	r.rangeFilesAll(f, seen)
}

func (r *NamespacedFiles) rangeFilesAll(f func(protoreflect.FileDescriptor) bool, seen map[string]struct{}) bool {
	if r == nil {
		return true
	}
	if r.isGlobalWrapper() {
		cont := true
		r.global.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
			if _, ok := seen[fd.Path()]; ok {
				return true
			}
			seen[fd.Path()] = struct{}{}
			cont = f(fd)
			return cont
		})
		return cont
	}
	r.mu.RLock()
	localFiles := make(map[string][]protoreflect.FileDescriptor, len(r.filesByPath))
	for k, v := range r.filesByPath {
		localFiles[k] = v
	}
	r.mu.RUnlock()

	for path, files := range localFiles {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		for _, file := range files {
			if !f(file) {
				return false
			}
		}
	}
	return r.parent.rangeFilesAll(f, seen)
}

// NumFilesByPackage reports the number of files in a proto package in the local namespace.
func (r *NamespacedFiles) NumFilesByPackage(name protoreflect.FullName) int {
	if r == nil {
		return 0
	}
	if r.isGlobalWrapper() {
		return r.global.NumFilesByPackage(name)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.descsByName[name].(*packageDescriptor)
	if !ok {
		return 0
	}
	return len(p.files)
}

// RangeFilesByPackage iterates over files in a given package in the local namespace.
func (r *NamespacedFiles) RangeFilesByPackage(name protoreflect.FullName, f func(protoreflect.FileDescriptor) bool) {
	if r == nil {
		return
	}
	if r.isGlobalWrapper() {
		r.global.RangeFilesByPackage(name, f)
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.descsByName[name].(*packageDescriptor)
	if !ok {
		return
	}
	for _, file := range p.files {
		if !f(file) {
			return
		}
	}
}

func removeFileByPath(files []protoreflect.FileDescriptor, path string) []protoreflect.FileDescriptor {
	for i, f := range files {
		if f.Path() == path {
			return append(files[:i], files[i+1:]...)
		}
	}
	return files
}

// NamespacedTypes is a type registry with hierarchical fallback.
// Mutations affect only the local registry. Lookups check the local registry
// first, then walk the parent chain.
//
// Safe for concurrent use.
type NamespacedTypes struct {
	mu     sync.RWMutex
	parent *NamespacedTypes

	// global is non-nil only for the read-only wrapper around protoregistry.GlobalTypes.
	global *protoregistry.Types

	typesByName         typesByName
	extensionsByMessage extensionsByMessage

	numEnums      int
	numMessages   int
	numExtensions int
}

var (
	_ protoregistry.MessageTypeResolver   = (*NamespacedTypes)(nil)
	_ protoregistry.ExtensionTypeResolver = (*NamespacedTypes)(nil)
)

// NewNamespacedTypes creates a new NamespacedTypes whose lookups fall back to parent.
// Pass nil to create a root namespace with no fallback.
func NewNamespacedTypes(parent *NamespacedTypes) *NamespacedTypes {
	return &NamespacedTypes{
		parent:              parent,
		typesByName:         make(typesByName),
		extensionsByMessage: make(extensionsByMessage),
	}
}

func (r *NamespacedTypes) isGlobalWrapper() bool {
	return r.global != nil
}

// RegisterMessage registers the provided message type in the local namespace.
func (r *NamespacedTypes) RegisterMessage(mt protoreflect.MessageType) error {
	if r.isGlobalWrapper() {
		return errors.New("nsregistry: cannot register types on read-only global namespace")
	}
	md := mt.Descriptor()
	r.mu.Lock()
	defer r.mu.Unlock()
	name := md.FullName()
	if r.typesByName[name] != nil {
		return fmt.Errorf("nsregistry: message %v is already registered", name)
	}
	r.typesByName[name] = mt
	r.numMessages++
	return nil
}

// UpdateMessage registers or replaces a message type (upsert).
func (r *NamespacedTypes) UpdateMessage(mt protoreflect.MessageType) error {
	if r.isGlobalWrapper() {
		return errors.New("nsregistry: cannot update types on read-only global namespace")
	}
	md := mt.Descriptor()
	r.mu.Lock()
	defer r.mu.Unlock()
	name := md.FullName()
	if r.typesByName[name] == nil {
		r.numMessages++
	}
	r.typesByName[name] = mt
	return nil
}

// UnregisterMessage removes a message type by full name.
// Returns protoregistry.NotFound if not registered locally.
func (r *NamespacedTypes) UnregisterMessage(name protoreflect.FullName) error {
	if r.isGlobalWrapper() {
		return errors.New("nsregistry: cannot unregister types on read-only global namespace")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.typesByName[name].(protoreflect.MessageType); !ok {
		return protoregistry.NotFound
	}
	delete(r.typesByName, name)
	r.numMessages--
	return nil
}

// RegisterEnum registers the provided enum type in the local namespace.
func (r *NamespacedTypes) RegisterEnum(et protoreflect.EnumType) error {
	if r.isGlobalWrapper() {
		return errors.New("nsregistry: cannot register types on read-only global namespace")
	}
	ed := et.Descriptor()
	r.mu.Lock()
	defer r.mu.Unlock()
	name := ed.FullName()
	if r.typesByName[name] != nil {
		return fmt.Errorf("nsregistry: enum %v is already registered", name)
	}
	r.typesByName[name] = et
	r.numEnums++
	return nil
}

// UpdateEnum registers or replaces an enum type (upsert).
func (r *NamespacedTypes) UpdateEnum(et protoreflect.EnumType) error {
	if r.isGlobalWrapper() {
		return errors.New("nsregistry: cannot update types on read-only global namespace")
	}
	ed := et.Descriptor()
	r.mu.Lock()
	defer r.mu.Unlock()
	name := ed.FullName()
	if r.typesByName[name] == nil {
		r.numEnums++
	}
	r.typesByName[name] = et
	return nil
}

// UnregisterEnum removes an enum type by full name.
// Returns protoregistry.NotFound if not registered locally.
func (r *NamespacedTypes) UnregisterEnum(name protoreflect.FullName) error {
	if r.isGlobalWrapper() {
		return errors.New("nsregistry: cannot unregister types on read-only global namespace")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.typesByName[name].(protoreflect.EnumType); !ok {
		return protoregistry.NotFound
	}
	delete(r.typesByName, name)
	r.numEnums--
	return nil
}

// RegisterExtension registers the provided extension type in the local namespace.
func (r *NamespacedTypes) RegisterExtension(xt protoreflect.ExtensionType) error {
	if r.isGlobalWrapper() {
		return errors.New("nsregistry: cannot register types on read-only global namespace")
	}
	xd := xt.TypeDescriptor()
	r.mu.Lock()
	defer r.mu.Unlock()

	name := xd.FullName()
	if r.typesByName[name] != nil {
		return fmt.Errorf("nsregistry: extension %v is already registered", name)
	}
	field := xd.Number()
	message := xd.ContainingMessage().FullName()
	if prev := r.extensionsByMessage[message][field]; prev != nil {
		return fmt.Errorf("nsregistry: extension number %d is already registered on message %v", field, message)
	}

	r.typesByName[name] = xt
	if r.extensionsByMessage[message] == nil {
		r.extensionsByMessage[message] = make(extensionsByNumber)
	}
	r.extensionsByMessage[message][field] = xt
	r.numExtensions++
	return nil
}

// UpdateExtension registers or replaces an extension type (upsert).
func (r *NamespacedTypes) UpdateExtension(xt protoreflect.ExtensionType) error {
	if r.isGlobalWrapper() {
		return errors.New("nsregistry: cannot update types on read-only global namespace")
	}
	xd := xt.TypeDescriptor()
	r.mu.Lock()
	defer r.mu.Unlock()

	name := xd.FullName()
	if r.typesByName[name] == nil {
		r.numExtensions++
	}
	r.typesByName[name] = xt
	field := xd.Number()
	message := xd.ContainingMessage().FullName()
	if r.extensionsByMessage[message] == nil {
		r.extensionsByMessage[message] = make(extensionsByNumber)
	}
	r.extensionsByMessage[message][field] = xt
	return nil
}

// UnregisterExtension removes an extension type by full name.
// Returns protoregistry.NotFound if not registered locally.
func (r *NamespacedTypes) UnregisterExtension(name protoreflect.FullName) error {
	if r.isGlobalWrapper() {
		return errors.New("nsregistry: cannot unregister types on read-only global namespace")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	xt, ok := r.typesByName[name].(protoreflect.ExtensionType)
	if !ok {
		return protoregistry.NotFound
	}
	xd := xt.TypeDescriptor()
	message := xd.ContainingMessage().FullName()
	field := xd.Number()
	delete(r.typesByName, name)
	if exts := r.extensionsByMessage[message]; exts != nil {
		delete(exts, field)
		if len(exts) == 0 {
			delete(r.extensionsByMessage, message)
		}
	}
	r.numExtensions--
	return nil
}

// FindEnumByName looks up an enum by its full name.
// Checks the local namespace first, then the parent chain.
//
// This returns (nil, protoregistry.NotFound) if not found.
func (r *NamespacedTypes) FindEnumByName(enum protoreflect.FullName) (protoreflect.EnumType, error) {
	if r == nil {
		return nil, protoregistry.NotFound
	}
	if r.isGlobalWrapper() {
		return r.global.FindEnumByName(enum)
	}
	r.mu.RLock()
	v := r.typesByName[enum]
	r.mu.RUnlock()
	if v != nil {
		if et, _ := v.(protoreflect.EnumType); et != nil {
			return et, nil
		}
		return nil, fmt.Errorf("nsregistry: found wrong type: got %v, want enum", typeName(v))
	}
	return r.parent.FindEnumByName(enum)
}

// FindMessageByName looks up a message by its full name.
// Checks the local namespace first, then the parent chain.
//
// This returns (nil, protoregistry.NotFound) if not found.
func (r *NamespacedTypes) FindMessageByName(message protoreflect.FullName) (protoreflect.MessageType, error) {
	if r == nil {
		return nil, protoregistry.NotFound
	}
	if r.isGlobalWrapper() {
		return r.global.FindMessageByName(message)
	}
	r.mu.RLock()
	v := r.typesByName[message]
	r.mu.RUnlock()
	if v != nil {
		if mt, _ := v.(protoreflect.MessageType); mt != nil {
			return mt, nil
		}
		return nil, fmt.Errorf("nsregistry: found wrong type: got %v, want message", typeName(v))
	}
	return r.parent.FindMessageByName(message)
}

// FindMessageByURL looks up a message by a URL identifier.
// Checks the local namespace first, then the parent chain.
//
// This returns (nil, protoregistry.NotFound) if not found.
func (r *NamespacedTypes) FindMessageByURL(url string) (protoreflect.MessageType, error) {
	if r == nil {
		return nil, protoregistry.NotFound
	}
	if r.isGlobalWrapper() {
		return r.global.FindMessageByURL(url)
	}
	message := protoreflect.FullName(url)
	if i := strings.LastIndexByte(url, '/'); i >= 0 {
		message = message[i+len("/"):]
	}
	r.mu.RLock()
	v := r.typesByName[message]
	r.mu.RUnlock()
	if v != nil {
		if mt, _ := v.(protoreflect.MessageType); mt != nil {
			return mt, nil
		}
		return nil, fmt.Errorf("nsregistry: found wrong type: got %v, want message", typeName(v))
	}
	return r.parent.FindMessageByURL(url)
}

// FindExtensionByName looks up an extension field by the field's full name.
// Checks the local namespace first, then the parent chain.
//
// This returns (nil, protoregistry.NotFound) if not found.
func (r *NamespacedTypes) FindExtensionByName(field protoreflect.FullName) (protoreflect.ExtensionType, error) {
	if r == nil {
		return nil, protoregistry.NotFound
	}
	if r.isGlobalWrapper() {
		return r.global.FindExtensionByName(field)
	}
	r.mu.RLock()
	v := r.typesByName[field]
	r.mu.RUnlock()
	if v != nil {
		if xt, _ := v.(protoreflect.ExtensionType); xt != nil {
			return xt, nil
		}

		return nil, fmt.Errorf("nsregistry: found wrong type: got %v, want extension", typeName(v))
	}
	return r.parent.FindExtensionByName(field)
}

// FindExtensionByNumber looks up an extension field by the field number
// within some parent message. Checks the local namespace first, then the parent chain.
//
// This returns (nil, protoregistry.NotFound) if not found.
func (r *NamespacedTypes) FindExtensionByNumber(message protoreflect.FullName, field protoreflect.FieldNumber) (protoreflect.ExtensionType, error) {
	if r == nil {
		return nil, protoregistry.NotFound
	}
	if r.isGlobalWrapper() {
		return r.global.FindExtensionByNumber(message, field)
	}
	r.mu.RLock()
	xt, ok := r.extensionsByMessage[message][field]
	r.mu.RUnlock()
	if ok {
		return xt, nil
	}
	return r.parent.FindExtensionByNumber(message, field)
}

// NumEnums reports the number of enums registered in the local namespace.
func (r *NamespacedTypes) NumEnums() int {
	if r == nil {
		return 0
	}
	if r.isGlobalWrapper() {
		return r.global.NumEnums()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.numEnums
}

// NumMessages reports the number of messages registered in the local namespace.
func (r *NamespacedTypes) NumMessages() int {
	if r == nil {
		return 0
	}
	if r.isGlobalWrapper() {
		return r.global.NumMessages()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.numMessages
}

// NumExtensions reports the number of extensions registered in the local namespace.
func (r *NamespacedTypes) NumExtensions() int {
	if r == nil {
		return 0
	}
	if r.isGlobalWrapper() {
		return r.global.NumExtensions()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.numExtensions
}

// RangeMessages iterates over messages in the local namespace while f returns true.
func (r *NamespacedTypes) RangeMessages(f func(protoreflect.MessageType) bool) {
	if r == nil {
		return
	}
	if r.isGlobalWrapper() {
		r.global.RangeMessages(f)
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, typ := range r.typesByName {
		if mt, ok := typ.(protoreflect.MessageType); ok {
			if !f(mt) {
				return
			}
		}
	}
}

// RangeEnums iterates over enums in the local namespace while f returns true.
func (r *NamespacedTypes) RangeEnums(f func(protoreflect.EnumType) bool) {
	if r == nil {
		return
	}
	if r.isGlobalWrapper() {
		r.global.RangeEnums(f)
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, typ := range r.typesByName {
		if et, ok := typ.(protoreflect.EnumType); ok {
			if !f(et) {
				return
			}
		}
	}
}

// RangeExtensions iterates over extensions in the local namespace while f returns true.
func (r *NamespacedTypes) RangeExtensions(f func(protoreflect.ExtensionType) bool) {
	if r == nil {
		return
	}
	if r.isGlobalWrapper() {
		r.global.RangeExtensions(f)
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, typ := range r.typesByName {
		if xt, ok := typ.(protoreflect.ExtensionType); ok {
			if !f(xt) {
				return
			}
		}
	}
}

// NumExtensionsByMessage reports the number of extensions for a given message
// in the local namespace.
func (r *NamespacedTypes) NumExtensionsByMessage(message protoreflect.FullName) int {
	if r == nil {
		return 0
	}
	if r.isGlobalWrapper() {
		return r.global.NumExtensionsByMessage(message)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.extensionsByMessage[message])
}

// RangeExtensionsByMessage iterates over extensions for a given message
// in the local namespace while f returns true.
func (r *NamespacedTypes) RangeExtensionsByMessage(message protoreflect.FullName, f func(protoreflect.ExtensionType) bool) {
	if r == nil {
		return
	}
	if r.isGlobalWrapper() {
		r.global.RangeExtensionsByMessage(message, f)
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, xt := range r.extensionsByMessage[message] {
		if !f(xt) {
			return
		}
	}
}
