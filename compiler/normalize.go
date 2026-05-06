// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
)

// NormalizeResult holds the output of normalizing a proto source file.
type NormalizeResult struct {
	// SHA256 is the hex-encoded SHA-256 hash of the normalized content.
	SHA256 string
	// OriginalSource is the unmodified source as submitted.
	OriginalSource []byte
}

// NormalizeAndHash parses a proto file, produces a canonical (normalized)
// representation by printing the AST without comments or extraneous whitespace,
// and returns the SHA-256 hash of that normalized form.
//
// The original source is returned unchanged for storage. The hash is used
// only for content-addressable deduplication and change detection.
func NormalizeAndHash(filename string, source []byte) (*NormalizeResult, error) {
	handler := reporter.NewHandler(nil)
	fileNode, err := parser.Parse(filename, strings.NewReader(string(source)), handler)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", filename, err)
	}

	h := sha256.New()
	writeCanonical(h, fileNode)

	return &NormalizeResult{
		SHA256:         hex.EncodeToString(h.Sum(nil)),
		OriginalSource: source,
	}, nil
}

// writeCanonical writes a deterministic representation of the AST to w,
// stripping comments and normalizing whitespace. The output is not valid
// .proto source — it's only used for hashing.
func writeCanonical(w io.Writer, file *ast.FileNode) {
	// Walk all non-comment tokens in order. This produces a deterministic
	// sequence regardless of original formatting, comment placement, or
	// whitespace variations.
	seq := file.Tokens()
	tok, ok := seq.First()
	for ok {
		info := file.TokenInfo(tok)
		// Skip comment-only whitespace differences; include all
		// semantically meaningful tokens separated by a single space.
		text := info.RawText()
		if text != "" {
			_, _ = fmt.Fprintf(w, "%s ", text)
		}
		tok, ok = seq.Next(tok)
	}
}
