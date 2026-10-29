// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package ctl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// extractImports lists a file's dependencies in declaration order,
// public and weak ones included.
func TestExtractImports(t *testing.T) {
	src := `syntax = "proto3";
package a.v1;
import "a/v1/plain.proto";
import public "a/v1/public.proto";
import weak "a/v1/weak.proto";
message M {}
`
	got, err := extractImports("a/v1/m.proto", []byte(src))
	require.NoError(t, err)
	assert.Equal(t, []string{"a/v1/plain.proto", "a/v1/public.proto", "a/v1/weak.proto"}, got)
}

// An edition 2024 `import option` names a file used only for custom
// options, not a dependency, so it is left out.
func TestExtractImports_ImportOptionIsNotADependency(t *testing.T) {
	src := `edition = "2024";
package a.v1;
import "a/v1/plain.proto";
import option "a/v1/opts.proto";
message M {}
`
	got, err := extractImports("a/v1/m.proto", []byte(src))
	require.NoError(t, err)
	assert.Equal(t, []string{"a/v1/plain.proto"}, got)
}

func TestExtractImports_NoImports(t *testing.T) {
	got, err := extractImports("b.proto", []byte("syntax = \"proto3\";\npackage b;\nmessage B {}\n"))
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestExtractImports_ParseError(t *testing.T) {
	_, err := extractImports("c.proto", []byte("syntax = \"proto3\";\nmessage {\n"))
	assert.Error(t, err)
}
