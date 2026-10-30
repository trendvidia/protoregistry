// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package compiler

import (
	"io"
	"io/fs"
	"slices"

	"github.com/trendvidia/protocompile"
	"github.com/trendvidia/protos"
)

// bundledImports are the canonical protowire schemas every namespace can
// import without publishing them, by import path. Without them here, a schema
// importing one would compile only after an operator published it to
// __builtins__.
//   - protowire/schema/v1/annotations.proto: the standard annotations an
//     RFC-001 schema (Protowire Schema Extensions) uses.
//   - pxf/annotations.proto: the PXF field options (pxf.required),
//     (pxf.default) and (pxf.key).
var bundledImports = []string{
	"protowire/schema/v1/annotations.proto",
	"pxf/annotations.proto",
}

// bundledResolver serves bundledImports from the protos module's embedded
// schemas, so the copy is the one in the protos version go.mod pins.
func bundledResolver() *protocompile.SourceResolver {
	return &protocompile.SourceResolver{
		Accessor: func(path string) (io.ReadCloser, error) {
			if !slices.Contains(bundledImports, path) {
				return nil, fs.ErrNotExist
			}
			return protos.Files.Open(path)
		},
	}
}
