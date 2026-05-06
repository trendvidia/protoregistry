// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import "testing"

func TestResolvePageSize(t *testing.T) {
	l := Limits{DefaultListPageSize: 100, MaxListPageSize: 1000}
	cases := map[string]struct {
		req  uint32
		want int
	}{
		"zero uses default":        {0, 100},
		"under max passes through": {50, 50},
		"at max passes through":    {1000, 1000},
		"over max is clamped":      {5000, 1000},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := l.resolvePageSize(tc.req); got != tc.want {
				t.Fatalf("resolvePageSize(%d) = %d, want %d", tc.req, got, tc.want)
			}
		})
	}
}

func TestDefaultLimitsSane(t *testing.T) {
	l := DefaultLimits()
	if l.MaxIDLength <= 0 || l.MaxFilenameLength <= 0 ||
		l.MaxSourcesPerRequest <= 0 || l.MaxTotalSourceBytes <= 0 ||
		l.MaxFileSourceBytes <= 0 || l.DefaultListPageSize <= 0 ||
		l.MaxListPageSize <= 0 {
		t.Fatalf("DefaultLimits has zero fields: %+v", l)
	}
	if l.MaxFileSourceBytes > l.MaxTotalSourceBytes {
		t.Fatalf("MaxFileSourceBytes (%d) > MaxTotalSourceBytes (%d)",
			l.MaxFileSourceBytes, l.MaxTotalSourceBytes)
	}
	if l.DefaultListPageSize > l.MaxListPageSize {
		t.Fatalf("DefaultListPageSize (%d) > MaxListPageSize (%d)",
			l.DefaultListPageSize, l.MaxListPageSize)
	}
}
