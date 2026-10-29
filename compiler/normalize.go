// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/parser"
	"github.com/trendvidia/protocompile/report"
	"github.com/trendvidia/protocompile/source"
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
	file, err := parseSource(filename, source)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", filename, err)
	}

	h := sha256.New()
	writeCanonical(h, file)

	return &NormalizeResult{
		SHA256:         hex.EncodeToString(h.Sum(nil)),
		OriginalSource: source,
	}, nil
}

// parseSource parses one file and returns its first error, if any.
// Warnings do not fail the parse.
func parseSource(filename string, text []byte) (*ast.File, error) {
	rpt := &report.Report{}
	file, _ := parser.Parse(filename, source.NewFile(filename, string(text)), rpt)
	for i := range rpt.Diagnostics {
		if d := &rpt.Diagnostics[i]; d.Level() <= report.Error {
			return nil, errors.New(d.Message())
		}
	}
	return file, nil
}

// writeCanonical writes a deterministic representation of the AST to w,
// stripping comments and normalizing whitespace. The output is not valid
// .proto source — it's only used for hashing.
func writeCanonical(w io.Writer, file *ast.File) {
	// Walk the non-skippable tokens (everything but whitespace and
	// comments) in order. This produces a deterministic sequence
	// regardless of original formatting, comment placement, or whitespace
	// variations, and the same one the pre-fork parser produced.
	for tok := range file.Stream().All() {
		if tok.IsSynthetic() || tok.Kind().IsSkippable() {
			continue
		}
		if text := tok.Text(); text != "" {
			_, _ = fmt.Fprintf(w, "%s ", text)
		}
	}
}
