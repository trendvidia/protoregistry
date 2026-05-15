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
	"google.golang.org/protobuf/reflect/protodesc"
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
//
// DepNamespaceID identifies which namespace contributed the file. For
// same-namespace dependencies (cross-schema imports within the publishing
// namespace) this equals the publishing namespace's ID. For cross-namespace
// dependencies resolved via the namespace hierarchy, this is the ancestor
// namespace that supplied the file. See docs/design/namespace-hierarchy.md
// decision D3.
type Dep struct {
	DepNamespaceID string
	DepSchemaID    string
	DepFilename    string
	DepVersion     uint64
}

// DepSource describes a file available to the compiler from another schema.
// Namespace identifies the contributing namespace. For same-namespace deps
// this equals the publishing namespace's ID; for parent-chain tiers this is
// the ancestor's ID.
type DepSource struct {
	Namespace string
	SchemaID  string
	Version   uint64
	Filename  string
	Source    []byte
}

// ChainTier is one tier of the parent-namespace resolver chain. Each tier
// represents one ancestor namespace's contribution to filename resolution.
// Tiers are ordered nearest-first by the caller; the compiler walks them
// in order and the first match wins (filename-based resolution per D7).
type ChainTier struct {
	// NamespaceID of this tier, for diagnostics and downstream attribution.
	NamespaceID string
	// Files contributed by this namespace at the pinned/current versions.
	Files []DepSource
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
//
// Parameters:
//   - sources: filename → content for the schema being compiled (the child).
//   - deps: files from other schemas in the *same* namespace, available for
//     import. Each DepSource's Namespace should equal the publishing namespace.
//   - parentChain: ancestor namespaces in resolution order (nearest first).
//     Each tier's files are tried after the child's own namespace tier.
//     Pass nil or empty for namespaces without a parent.
//   - builtins: files from the __builtins__ namespace, available to every
//     namespace; resolve after the parent chain but before Google WKT.
//
// Resolution order: child sources & same-ns deps → parent chain tiers
// (nearest first) → builtins → Google well-known types. First file whose
// path matches wins (decision D7).
//
// Compile does not perform cross-tier FQN collision detection (D2). Callers
// that need it should invoke DetectFQNConflicts on the resulting snapshot
// against each ancestor's snapshot.
func (c *Compiler) Compile(
	ctx context.Context,
	version uint64,
	sources map[string][]byte,
	deps []DepSource,
	parentChain []ChainTier,
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
	//   child namespace (own sources + deps)
	//     → parent chain (nearest first)
	//     → builtins
	//     → Google well-known types
	resolver := buildResolverChain(sources, deps, parentChain, builtins)

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

	// Build the file descriptor set for storage. protocompile.Compile only
	// returns the explicitly-requested files (the schema's own sources),
	// so we walk each one's import tree recursively to include every
	// transitively-imported file. Without this, the stored FDS would
	// reference parent-chain and same-namespace dep files by path but
	// not carry their definitions, and Restore's fast path would fail
	// with "could not resolve import". Well-known types are included as
	// well — protodesc.NewFiles doesn't fall back to GlobalFiles for
	// import resolution, so any WKT a schema imports needs to be in the
	// FDS too. The storage bloat is acceptable in exchange for the
	// invariant that stored compiled bytes are self-contained.
	fds := &descriptorpb.FileDescriptorSet{}
	var fileDescriptors []protoreflect.FileDescriptor
	seen := make(map[string]struct{})
	var addFile func(fd protoreflect.FileDescriptor)
	addFile = func(fd protoreflect.FileDescriptor) {
		if _, dup := seen[fd.Path()]; dup {
			return
		}
		seen[fd.Path()] = struct{}{}
		// Walk imports first so the FDS is in topological order
		// (dependencies before dependents).
		imports := fd.Imports()
		for i := range imports.Len() {
			addFile(imports.Get(i).FileDescriptor)
		}
		fileDescriptors = append(fileDescriptors, fd)
		if lr, ok := fd.(linker.Result); ok {
			fds.File = append(fds.File, lr.FileDescriptorProto())
		} else {
			fds.File = append(fds.File, protodesc.ToFileDescriptorProto(fd))
		}
	}
	for _, file := range compiled {
		addFile(file)
	}

	serialized, err := proto.Marshal(fds)
	if err != nil {
		return nil, fmt.Errorf("serializing descriptor set: %w", err)
	}

	snap, err := snapshot.NewFromFiles(version, fileDescriptors)
	if err != nil {
		return nil, fmt.Errorf("building snapshot: %w", err)
	}

	// Record dependencies used. Same-namespace deps and parent-chain
	// contributions both go into the result, distinguished by
	// DepNamespaceID. Callers (the registry) write these to
	// schema_version_deps where they form the per-import pin
	// (decision D3).
	var depRecords []Dep
	for _, d := range deps {
		depRecords = append(depRecords, Dep{
			DepNamespaceID: d.Namespace,
			DepSchemaID:    d.SchemaID,
			DepFilename:    d.Filename,
			DepVersion:     d.Version,
		})
	}
	for _, tier := range parentChain {
		for _, d := range tier.Files {
			depRecords = append(depRecords, Dep{
				DepNamespaceID: d.Namespace,
				DepSchemaID:    d.SchemaID,
				DepFilename:    d.Filename,
				DepVersion:     d.Version,
			})
		}
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

// buildResolverChain creates an N-tier resolver:
//
//  1. Child-namespace sources (own schema files + same-namespace deps)
//  2. Parent-chain tiers (one per ancestor, nearest first)
//  3. Built-in sources (the __builtins__ namespace)
//  4. Google well-known types (via protocompile.WithStandardImports, outermost)
//
// Tiers are walked in order; the first file whose path matches wins
// (decision D7 — filename-based chain resolution).
func buildResolverChain(
	sources map[string][]byte,
	deps []DepSource,
	parentChain []ChainTier,
	builtins []DepSource,
) protocompile.Resolver {
	tiers := make(protocompile.CompositeResolver, 0, 2+len(parentChain))

	// Tier 1: child namespace (own sources + same-namespace deps).
	tiers = append(tiers, sourceMapResolver(sources, deps))

	// Tier 2..N: parent chain (nearest first).
	for _, tier := range parentChain {
		if len(tier.Files) == 0 {
			continue
		}
		tiers = append(tiers, sourceMapResolver(nil, tier.Files))
	}

	// Tier N+1: builtins.
	if len(builtins) > 0 {
		tiers = append(tiers, sourceMapResolver(nil, builtins))
	}

	// Outermost: Google well-known types.
	return protocompile.WithStandardImports(tiers)
}

// sourceMapResolver builds a SourceResolver from the union of an optional
// child source map and a slice of DepSources. Used to construct each tier
// of the chain.
func sourceMapResolver(sources map[string][]byte, deps []DepSource) *protocompile.SourceResolver {
	merged := make(map[string]string, len(sources)+len(deps))
	for filename, content := range sources {
		merged[filename] = string(content)
	}
	for _, d := range deps {
		merged[d.Filename] = string(d.Source)
	}
	return &protocompile.SourceResolver{
		Accessor: protocompile.SourceAccessorFromMap(merged),
	}
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
