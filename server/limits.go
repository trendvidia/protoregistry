// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

// Limits caps the size and shape of incoming RPC payloads. Every limit is
// expressed in user units (bytes, count, length) so operators can reason
// about them directly. Limits are enforced at the RPC boundary before any
// expensive work (compilation, blob storage, descriptor materialization)
// happens — they are the first line of defence against DoS via crafted
// inputs.
//
// The compile-time wall-clock cap lives on the compiler (see
// compiler.WithTimeout). It is enforced where the cost is paid, and so
// applies whether the compiler is invoked from the server, a CLI command,
// or an embedded library caller.
type Limits struct {
	// MaxIDLength is the maximum length, in bytes, of a namespace ID,
	// schema ID, or token cursor. Applied to user-supplied identifiers only.
	MaxIDLength int

	// MaxFilenameLength is the maximum length, in bytes, of a source
	// filename inside a Publish request.
	MaxFilenameLength int

	// MaxSourcesPerRequest caps the number of source files in a single
	// Publish request. Each file allocates AST nodes during normalization,
	// so this cap bounds compile-time memory.
	MaxSourcesPerRequest int

	// MaxTotalSourceBytes caps the sum of all source file sizes in a
	// single Publish request.
	MaxTotalSourceBytes int64

	// MaxFileSourceBytes caps the size of any individual source file.
	MaxFileSourceBytes int64

	// DefaultListPageSize is used when a List* RPC sends page_size = 0.
	DefaultListPageSize int

	// MaxListPageSize is the upper bound the server will honour even if a
	// caller asks for more.
	MaxListPageSize int
}

// DefaultLimits returns the default Limits used when the server is
// constructed without WithLimits. The numbers are chosen to be permissive
// enough for normal use (multi-megabyte schemas, hundreds of files) while
// still bounding worst-case server memory.
func DefaultLimits() Limits {
	return Limits{
		MaxIDLength:          256,
		MaxFilenameLength:    512,
		MaxSourcesPerRequest: 1000,
		MaxTotalSourceBytes:  32 * 1024 * 1024, // 32 MiB
		MaxFileSourceBytes:   8 * 1024 * 1024,  // 8 MiB
		DefaultListPageSize:  100,
		MaxListPageSize:      1000,
	}
}

// resolvePageSize clamps a caller-requested page size to [1, MaxListPageSize],
// substituting DefaultListPageSize when the caller passes 0.
func (l Limits) resolvePageSize(requested uint32) int {
	if requested == 0 {
		return l.DefaultListPageSize
	}
	if int(requested) > l.MaxListPageSize {
		return l.MaxListPageSize
	}
	return int(requested)
}
