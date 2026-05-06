// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package compiler

import (
	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// IsWellKnownType reports whether filename matches a standard import
// provided by protocompile (e.g., google/protobuf/timestamp.proto).
func IsWellKnownType(filename string) bool {
	// protocompile.WithStandardImports wraps a resolver with the
	// standard imports from the protobuf-go runtime. We check if
	// the runtime's global file registry knows about this path.
	_, err := protoregistry.GlobalFiles.FindFileByPath(filename)
	if err == nil {
		return true
	}
	// Also check via protocompile's resolver, which may include
	// files not in the Go runtime (e.g., compiler/plugin.proto).
	r := protocompile.WithStandardImports(&protocompile.SourceResolver{})
	_, err = r.FindFileByPath(filename)
	return err == nil
}
