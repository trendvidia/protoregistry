// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package compiler

import (
	"context"
	"strings"
	"testing"
	"time"
)

// validProto is the smallest source the compiler accepts. Used by the
// limit tests so that a successful Compile path is reachable when the
// limit is the only thing under test.
const validProto = `syntax = "proto3";
package test;
message M { string name = 1; }
`

func TestCompileFileCountCap(t *testing.T) {
	c := New(WithMaxFiles(2))
	sources := map[string][]byte{
		"a.proto": []byte(validProto),
		"b.proto": []byte(validProto),
		"c.proto": []byte(validProto),
	}
	_, err := c.Compile(context.Background(), 1, sources, nil, nil)
	if err == nil {
		t.Fatal("expected file count cap error, got nil")
	}
	if !strings.Contains(err.Error(), "too many files") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestCompilePerFileSizeCap(t *testing.T) {
	c := New(WithMaxFileSourceBytes(16))
	// A 17-byte source is over the cap; the rejection runs before parsing.
	sources := map[string][]byte{
		"big.proto": []byte("a really long proto that exceeds 16 bytes"),
	}
	_, err := c.Compile(context.Background(), 1, sources, nil, nil)
	if err == nil {
		t.Fatal("expected per-file size cap error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestCompileTimeoutFires(t *testing.T) {
	// 1ns timeout is guaranteed to expire before protocompile can finish
	// any real work; we just want the deadline to trip.
	c := New(WithTimeout(1 * time.Nanosecond))
	sources := map[string][]byte{"a.proto": []byte(validProto)}
	_, err := c.Compile(context.Background(), 1, sources, nil, nil)
	if err == nil {
		t.Fatal("expected timeout-induced compile failure, got nil")
	}
	// protocompile reports the deadline as part of "compilation failed"
	// — we don't assert on the exact text since it depends on the
	// upstream library version.
}

func TestCompileTimeoutDisabled(t *testing.T) {
	// Sanity: WithTimeout(0) disables the wrapper, and a normal compile
	// still succeeds. Catches regressions where 0 was treated as "fire
	// immediately" rather than "no deadline".
	c := New(WithTimeout(0))
	sources := map[string][]byte{"a.proto": []byte(validProto)}
	if _, err := c.Compile(context.Background(), 1, sources, nil, nil); err != nil {
		t.Fatalf("compile should succeed with no timeout: %v", err)
	}
}
