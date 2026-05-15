// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package compiler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// NormalizeAndHash
// ---------------------------------------------------------------------------

func TestNormalizeAndHash_SameSemanticContent(t *testing.T) {
	const filename = "test.proto"

	compact := []byte(`syntax="proto3";package test;message Foo{string name=1;}`)

	spacious := []byte(`
		// A comment that should be ignored.
		syntax = "proto3";

		// Package-level comment.
		package test;

		// Message comment.
		message Foo {
			// Field comment.
			string name = 1;
		}
	`)

	r1, err := NormalizeAndHash(filename, compact)
	require.NoError(t, err)

	r2, err := NormalizeAndHash(filename, spacious)
	require.NoError(t, err)

	assert.Equal(t, r1.SHA256, r2.SHA256,
		"same semantic content with different formatting/comments must produce the same hash")
	assert.Equal(t, compact, r1.OriginalSource,
		"OriginalSource must be the unmodified input")
	assert.Equal(t, spacious, r2.OriginalSource)
}

func TestNormalizeAndHash_DifferentContent(t *testing.T) {
	const filename = "test.proto"

	src1 := []byte(`syntax="proto3";package a;message A{string x=1;}`)
	src2 := []byte(`syntax="proto3";package b;message B{int32 y=1;}`)

	r1, err := NormalizeAndHash(filename, src1)
	require.NoError(t, err)

	r2, err := NormalizeAndHash(filename, src2)
	require.NoError(t, err)

	assert.NotEqual(t, r1.SHA256, r2.SHA256,
		"semantically different content must produce different hashes")
}

func TestNormalizeAndHash_InvalidProto(t *testing.T) {
	_, err := NormalizeAndHash("bad.proto", []byte(`not valid proto at all !!!`))
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// ComputeFingerprint
// ---------------------------------------------------------------------------

func TestComputeFingerprint_DeterministicRegardlessOfOrder(t *testing.T) {
	files1 := []FileResult{
		{Filename: "a.proto", BlobSHA256: "aaa"},
		{Filename: "b.proto", BlobSHA256: "bbb"},
		{Filename: "c.proto", BlobSHA256: "ccc"},
	}
	files2 := []FileResult{
		{Filename: "c.proto", BlobSHA256: "ccc"},
		{Filename: "a.proto", BlobSHA256: "aaa"},
		{Filename: "b.proto", BlobSHA256: "bbb"},
	}

	fp1 := ComputeFingerprint(files1)
	fp2 := ComputeFingerprint(files2)

	assert.Equal(t, fp1, fp2,
		"fingerprint must be deterministic regardless of input order")
	assert.Len(t, fp1, 64, "SHA-256 hex digest must be 64 characters")
}

func TestComputeFingerprint_DetectsAddition(t *testing.T) {
	base := []FileResult{
		{Filename: "a.proto", BlobSHA256: "aaa"},
	}
	withExtra := []FileResult{
		{Filename: "a.proto", BlobSHA256: "aaa"},
		{Filename: "b.proto", BlobSHA256: "bbb"},
	}

	assert.NotEqual(t, ComputeFingerprint(base), ComputeFingerprint(withExtra),
		"adding a file must change the fingerprint")
}

func TestComputeFingerprint_DetectsRemoval(t *testing.T) {
	full := []FileResult{
		{Filename: "a.proto", BlobSHA256: "aaa"},
		{Filename: "b.proto", BlobSHA256: "bbb"},
	}
	partial := []FileResult{
		{Filename: "a.proto", BlobSHA256: "aaa"},
	}

	assert.NotEqual(t, ComputeFingerprint(full), ComputeFingerprint(partial),
		"removing a file must change the fingerprint")
}

func TestComputeFingerprint_DetectsContentChange(t *testing.T) {
	before := []FileResult{
		{Filename: "a.proto", BlobSHA256: "aaa"},
	}
	after := []FileResult{
		{Filename: "a.proto", BlobSHA256: "zzz"},
	}

	assert.NotEqual(t, ComputeFingerprint(before), ComputeFingerprint(after),
		"changing a file's hash must change the fingerprint")
}

// ---------------------------------------------------------------------------
// Compile
// ---------------------------------------------------------------------------

func TestCompile_SimpleProto(t *testing.T) {
	c := New()

	sources := map[string][]byte{
		"test.proto": []byte(`
syntax = "proto3";
package test;
message Config {
  string name = 1;
  int32 value = 2;
}
`),
	}

	result, err := c.Compile(context.Background(), 1, sources, nil, nil, nil)
	require.NoError(t, err)

	// Snapshot should be non-nil and carry the correct version.
	require.NotNil(t, result.Snapshot, "Snapshot must not be nil")
	assert.Equal(t, uint64(1), result.Snapshot.Version())

	// Compiled bytes must be non-empty (serialized FileDescriptorSet).
	assert.NotEmpty(t, result.Compiled, "Compiled bytes must not be empty")

	// Files must have exactly one entry with a valid hash.
	require.Len(t, result.Files, 1)
	assert.Equal(t, "test.proto", result.Files[0].Filename)
	assert.Len(t, result.Files[0].BlobSHA256, 64, "BlobSHA256 must be a 64-char hex string")
	assert.NotEmpty(t, result.Files[0].OriginalSource)

	// The snapshot should know about the Config message.
	md, err := result.Snapshot.FindMessageByName("test.Config")
	require.NoError(t, err)
	assert.Equal(t, "Config", string(md.Name()))
	assert.Equal(t, 2, md.Fields().Len())
}

func TestCompile_InvalidProto(t *testing.T) {
	c := New()

	sources := map[string][]byte{
		"bad.proto": []byte(`this is not valid protobuf`),
	}

	_, err := c.Compile(context.Background(), 1, sources, nil, nil, nil)
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// Version
// ---------------------------------------------------------------------------

func TestVersion_NonEmpty(t *testing.T) {
	v := Version()
	assert.NotEmpty(t, v, "Version must return a non-empty string")
	assert.Contains(t, v, "protocompile@")
	assert.Contains(t, v, "protobuf-go@")
}

// ---------------------------------------------------------------------------
// IsWellKnownType
// ---------------------------------------------------------------------------

func TestIsWellKnownType(t *testing.T) {
	assert.True(t, IsWellKnownType("google/protobuf/timestamp.proto"))
	assert.True(t, IsWellKnownType("google/protobuf/descriptor.proto"))
	assert.True(t, IsWellKnownType("google/protobuf/empty.proto"))
	assert.True(t, IsWellKnownType("google/protobuf/any.proto"))
	assert.True(t, IsWellKnownType("google/protobuf/wrappers.proto"))

	assert.False(t, IsWellKnownType("mycompany/types.proto"))
	assert.False(t, IsWellKnownType("foo.proto"))
	assert.False(t, IsWellKnownType("google/protobuf/nonexistent.proto"))
}

// ---------------------------------------------------------------------------
// Compile with builtins
// ---------------------------------------------------------------------------

func TestCompile_WithBuiltins(t *testing.T) {
	c := New()

	builtins := []DepSource{
		{
			Filename: "company/base.proto",
			Source:   []byte(`syntax = "proto3"; package company; message BaseEntity { string id = 1; }`),
		},
	}

	sources := map[string][]byte{
		"order.proto": []byte(`
syntax = "proto3";
package order;
import "company/base.proto";
message Order {
  string name = 1;
  company.BaseEntity entity = 2;
}
`),
	}

	result, err := c.Compile(context.Background(), 1, sources, nil, nil, builtins)
	require.NoError(t, err)
	require.NotNil(t, result.Snapshot)

	md, err := result.Snapshot.FindMessageByName("order.Order")
	require.NoError(t, err)
	assert.Equal(t, 2, md.Fields().Len())
}
