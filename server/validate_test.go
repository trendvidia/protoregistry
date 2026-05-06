// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestValidateID(t *testing.T) {
	cases := []struct {
		name    string
		id      string
		wantErr bool
		errCode codes.Code
	}{
		{"empty", "", true, codes.InvalidArgument},
		{"single letter", "a", false, codes.OK},
		{"alphanumeric", "billing-v2.1_alpha", false, codes.OK},
		{"max length", strings.Repeat("a", 256), false, codes.OK},
		{"over max length", strings.Repeat("a", 257), true, codes.InvalidArgument},
		{"starts with underscore", "_internal", true, codes.InvalidArgument},
		{"contains slash", "billing/v2", true, codes.InvalidArgument},
		{"contains space", "billing v2", true, codes.InvalidArgument},
		{"contains traversal", "../etc/passwd", true, codes.InvalidArgument},
		{"contains NUL", "bill\x00ing", true, codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateID(tc.id, "test_field", 256)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err != nil {
				st, ok := status.FromError(err)
				if !ok || st.Code() != tc.errCode {
					t.Fatalf("expected gRPC code %v, got %v", tc.errCode, err)
				}
			}
		})
	}
}

func TestValidateFilename(t *testing.T) {
	cases := []struct {
		name    string
		fn      string
		wantErr bool
	}{
		{"simple", "billing.proto", false},
		{"nested", "billing/config.proto", false},
		{"empty", "", true},
		{"absolute", "/etc/passwd", true},
		{"traversal", "../../etc/passwd", true},
		{"trailing slash", "billing/", true},
		{"redundant dot", "./billing.proto", true},
		{"contains NUL", "billing\x00.proto", true},
		{"deep traversal segment", "billing/../config.proto", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFilename(tc.fn, 512)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tc.fn)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.fn, err)
			}
		})
	}
}

func TestValidateFilenameLength(t *testing.T) {
	long := strings.Repeat("a", 513)
	if err := validateFilename(long, 512); err == nil {
		t.Fatal("expected length error for 513-byte filename")
	}
}

func TestValidateSources(t *testing.T) {
	limits := DefaultLimits()

	t.Run("empty", func(t *testing.T) {
		if err := validateSources(map[string][]byte{}, limits); err == nil {
			t.Fatal("expected error for empty sources")
		}
	})

	t.Run("one valid file", func(t *testing.T) {
		err := validateSources(map[string][]byte{"a.proto": []byte("syntax = \"proto3\";")}, limits)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("file count cap", func(t *testing.T) {
		l := limits
		l.MaxSourcesPerRequest = 2
		sources := map[string][]byte{
			"a.proto": []byte("a"),
			"b.proto": []byte("b"),
			"c.proto": []byte("c"),
		}
		if err := validateSources(sources, l); err == nil {
			t.Fatal("expected file count error")
		}
	})

	t.Run("per-file size cap", func(t *testing.T) {
		l := limits
		l.MaxFileSourceBytes = 4
		sources := map[string][]byte{"big.proto": []byte("hello world")}
		if err := validateSources(sources, l); err == nil {
			t.Fatal("expected per-file size error")
		}
	})

	t.Run("total size cap", func(t *testing.T) {
		l := limits
		l.MaxFileSourceBytes = 1024
		l.MaxTotalSourceBytes = 8
		sources := map[string][]byte{
			"a.proto": []byte("12345"),
			"b.proto": []byte("12345"),
		}
		if err := validateSources(sources, l); err == nil {
			t.Fatal("expected total size error")
		}
	})

	t.Run("bad filename rejected", func(t *testing.T) {
		sources := map[string][]byte{"../oops.proto": []byte("a")}
		if err := validateSources(sources, limits); err == nil {
			t.Fatal("expected traversal rejection")
		}
	})
}
