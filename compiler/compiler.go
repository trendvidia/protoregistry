// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package compiler wraps protocompile to provide namespace-scoped compilation
// of proto source files. It handles normalization, SHA-256 hashing for
// content-addressable storage, and builds the resolver chain that enforces
// namespace isolation (chroot model).
package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/linker"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/trendvidia/protoregistry/snapshot"
)

// Default resource caps. Compile is the user-facing entry point and these
// numbers bound worst-case CPU and memory consumption per call. The server
// applies tighter validation upstream; these are the library-level safety
// net so the compiler is also safe to embed in non-server callers.
const (
	// DefaultCompileTimeout caps wall-clock time spent inside protocompile.
	DefaultCompileTimeout = 30 * time.Second

	// DefaultMaxFileSourceBytes caps the size of any individual proto
	// source the compiler will accept. Enforced before any AST work.
	DefaultMaxFileSourceBytes = 8 * 1024 * 1024 // 8 MiB

	// DefaultMaxFiles caps the number of files in a single Compile call.
	DefaultMaxFiles = 1000
)

// CompileResult holds the output of a compilation.
type CompileResult struct {
	// Snapshot is the compiled, immutable descriptor set.
	Snapshot *snapshot.Snapshot
	// Compiled is the serialized FileDescriptorSet for storage.
	Compiled []byte
	// Files is the list of normalized file hashes, for blob storage.
	Files []FileResult
	// Deps records which dependency versions were used during compilation.
	Deps []Dep
}

// FileResult associates a filename with its content hash after normalization.
type FileResult struct {
	Filename       string
	BlobSHA256     string
	OriginalSource []byte
}

// Dep records a dependency on another schema's file used during compilation.
type Dep struct {
	DepSchemaID string
	DepFilename string
	DepVersion  uint64
}

// DepSource describes a file available from another schema in the namespace.
// These are provided to the compiler so it can resolve cross-schema imports.
type DepSource struct {
	SchemaID string
	Version  uint64
	Filename string
	Source   []byte
}

// Compiler wraps protocompile for namespace-scoped compilation.
type Compiler struct {
	timeout            time.Duration
	maxFileSourceBytes int64
	maxFiles           int
}

// CompilerOption customises a Compiler.
type CompilerOption func(*Compiler)

// WithTimeout sets the wall-clock cap on a single Compile call. Pass 0 to
// disable (not recommended in server contexts).
func WithTimeout(d time.Duration) CompilerOption {
	return func(c *Compiler) { c.timeout = d }
}

// WithMaxFileSourceBytes sets the per-file size cap.
func WithMaxFileSourceBytes(n int64) CompilerOption {
	return func(c *Compiler) { c.maxFileSourceBytes = n }
}

// WithMaxFiles sets the cap on the number of files per Compile call.
func WithMaxFiles(n int) CompilerOption {
	return func(c *Compiler) { c.maxFiles = n }
}

// New creates a new Compiler with default caps. Pass options to override.
func New(opts ...CompilerOption) *Compiler {
	c := &Compiler{
		timeout:            DefaultCompileTimeout,
		maxFileSourceBytes: DefaultMaxFileSourceBytes,
		maxFiles:           DefaultMaxFiles,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Compile compiles the given proto sources within a namespace scope.
// The sources map is filename → content for the schema being compiled.
// The deps slice provides files from other schemas in the namespace that
// may be imported. The builtins slice provides files from the built-ins
// namespace that are available to all namespaces; they resolve after
// namespace sources but before Google well-known types.
func (c *Compiler) Compile(
	ctx context.Context,
	version uint64,
	sources map[string][]byte,
	deps []DepSource,
	builtins []DepSource,
) (*CompileResult, error) {
	// Up-front bounds checks. These run before any AST work so a hostile
	// caller cannot allocate more than O(input size) before the rejection.
	if c.maxFiles > 0 && len(sources) > c.maxFiles {
		return nil, fmt.Errorf("too many files (%d > %d)", len(sources), c.maxFiles)
	}
	if c.maxFileSourceBytes > 0 {
		for filename, source := range sources {
			if int64(len(source)) > c.maxFileSourceBytes {
				return nil, fmt.Errorf("source %q exceeds %d byte limit (%d bytes)",
					filename, c.maxFileSourceBytes, len(source))
			}
		}
	}

	// Apply the compile-time deadline. If the caller's context already has
	// a tighter deadline, that wins (context.WithTimeout respects whichever
	// is sooner).
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	// Normalize and hash each source file.
	fileResults := make([]FileResult, 0, len(sources))
	for filename, source := range sources {
		nr, err := NormalizeAndHash(filename, source)
		if err != nil {
			return nil, fmt.Errorf("normalizing %s: %w", filename, err)
		}
		fileResults = append(fileResults, FileResult{
			Filename:       filename,
			BlobSHA256:     nr.SHA256,
			OriginalSource: nr.OriginalSource,
		})
	}

	// Build the resolver chain:
	//   namespace (own sources + deps) → builtins → Google well-known types
	resolver := buildResolverChain(sources, deps, builtins)

	// Collect filenames to compile (only the schema's own files).
	filenames := make([]string, 0, len(sources))
	for filename := range sources {
		filenames = append(filenames, filename)
	}

	// Compile.
	compiler := &protocompile.Compiler{
		Resolver: resolver,
	}
	compiled, err := compiler.Compile(ctx, filenames...)
	if err != nil {
		return nil, fmt.Errorf("compilation failed: %w", err)
	}

	// Build the file descriptor set for storage.
	fds := &descriptorpb.FileDescriptorSet{}
	var fileDescriptors []protoreflect.FileDescriptor
	for _, file := range compiled {
		fileDescriptors = append(fileDescriptors, file)
		if lr, ok := file.(linker.Result); ok {
			fds.File = append(fds.File, lr.FileDescriptorProto())
		}
	}

	serialized, err := proto.Marshal(fds)
	if err != nil {
		return nil, fmt.Errorf("serializing descriptor set: %w", err)
	}

	snap, err := snapshot.NewFromFiles(version, fileDescriptors)
	if err != nil {
		return nil, fmt.Errorf("building snapshot: %w", err)
	}

	// Record dependencies used.
	var depRecords []Dep
	for _, d := range deps {
		depRecords = append(depRecords, Dep{
			DepSchemaID: d.SchemaID,
			DepFilename: d.Filename,
			DepVersion:  d.Version,
		})
	}

	return &CompileResult{
		Snapshot: snap,
		Compiled: serialized,
		Files:    fileResults,
		Deps:     depRecords,
	}, nil
}

// Version returns a string identifying the compiler and its dependencies,
// used to detect when cached compiled descriptors may be stale.
func Version() string {
	var protocompileVersion, protobufVersion string
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			switch dep.Path {
			case "github.com/bufbuild/protocompile":
				protocompileVersion = dep.Version
			case "google.golang.org/protobuf":
				protobufVersion = dep.Version
			}
		}
	}
	return fmt.Sprintf("protocompile@%s+protobuf-go@%s", protocompileVersion, protobufVersion)
}

// buildResolverChain creates the 3-tier resolver:
//  1. Namespace sources (own schema files + deps from other schemas)
//  2. Built-in sources (files from the __builtins__ namespace)
//  3. Google well-known types (via protocompile.WithStandardImports)
func buildResolverChain(sources map[string][]byte, deps, builtins []DepSource) protocompile.Resolver {
	// Tier 1: namespace sources (own + deps).
	nsSources := make(map[string]string, len(sources)+len(deps))
	for filename, content := range sources {
		nsSources[filename] = string(content)
	}
	for _, d := range deps {
		nsSources[d.Filename] = string(d.Source)
	}
	nsResolver := &protocompile.SourceResolver{
		Accessor: protocompile.SourceAccessorFromMap(nsSources),
	}

	if len(builtins) == 0 {
		return protocompile.WithStandardImports(nsResolver)
	}

	// Tier 2: built-in sources.
	biSources := make(map[string]string, len(builtins))
	for _, b := range builtins {
		biSources[b.Filename] = string(b.Source)
	}
	biResolver := &protocompile.SourceResolver{
		Accessor: protocompile.SourceAccessorFromMap(biSources),
	}

	// Tier 3 (outermost): Google well-known types.
	return protocompile.WithStandardImports(
		protocompile.CompositeResolver{nsResolver, biResolver},
	)
}

// ComputeFingerprint computes a fingerprint over a sorted set of
// (filename, hash) pairs. Used to detect whether a new submission
// actually changes anything compared to the current version.
func ComputeFingerprint(files []FileResult) string {
	// Sort by filename for determinism.
	sorted := make([]FileResult, len(files))
	copy(sorted, files)
	sortFileResults(sorted)

	var b strings.Builder
	for _, f := range sorted {
		fmt.Fprintf(&b, "%s:%s\n", f.Filename, f.BlobSHA256)
	}
	return sha256Hex([]byte(b.String()))
}

func sortFileResults(files []FileResult) {
	sort.Slice(files, func(i, j int) bool {
		return files[i].Filename < files[j].Filename
	})
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
