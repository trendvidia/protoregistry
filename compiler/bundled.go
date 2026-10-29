// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package compiler

import (
	"io"
	"io/fs"

	"github.com/trendvidia/protocompile"
	"github.com/trendvidia/protowire"
)

// bundledImports are the canonical protowire schemas every namespace can
// import without publishing them, by import path, with their place in
// protowire.BundledProto. An RFC-001 schema (Protowire Schema Extensions)
// imports protowire/schema/v1/annotations.proto for the standard
// annotations; without it here, such a schema would compile only after an
// operator published the file to __builtins__.
var bundledImports = map[string]string{
	"protowire/schema/v1/annotations.proto": "proto/schema/v1/annotations.proto",
}

// bundledResolver serves bundledImports from the protowire module's
// embedded proto tree, so the copy is the one in the protowire version
// go.mod pins.
func bundledResolver() *protocompile.SourceResolver {
	return &protocompile.SourceResolver{
		Accessor: func(path string) (io.ReadCloser, error) {
			embedded, ok := bundledImports[path]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return protowire.BundledProto.Open(embedded)
		},
	}
}
