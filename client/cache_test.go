// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package client_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protoregistry/client"
	"github.com/trendvidia/protoregistry/client/internal/clienttest"
)

// TestDiskCacheOfflineReload spins up a server, populates a cache,
// then constructs a fresh Resolver via Dial against an unreachable
// address and verifies it falls back to the cache and serves the
// previously-persisted descriptors.
func TestDiskCacheOfflineReload(t *testing.T) {
	srv := clienttest.Start(t)
	const ns = "offline-roundtrip"
	const proto = `syntax = "proto3"; package billing; message Config { string name = 1; int32 timeout_ms = 2; }`
	srv.PublishAndPromote(t, ns, "billing", map[string][]byte{
		"billing.proto": []byte(proto),
	})

	cacheDir := t.TempDir()
	// Online: populate the cache.
	online, err := client.New(t.Context(), srv.Conn, ns,
		client.WithRefreshInterval(0),
		client.WithDiskCache(cacheDir),
	)
	require.NoError(t, err)
	require.False(t, online.IsStale())
	_ = online.Close()

	require.FileExists(t, filepath.Join(cacheDir, ns, "manifest.json"))

	// Offline: Dial an address that won't connect. Expect fallback
	// to the cached snapshot. Use 127.0.0.1:1 — guaranteed-closed
	// loopback port; fails fast with "connection refused" on every
	// platform.
	offline, err := client.Dial(t.Context(), "127.0.0.1:1", ns,
		client.WithRefreshInterval(0),
		client.WithDiskCache(cacheDir),
	)
	require.NoError(t, err, "offline Dial must fall back to cache, not error")
	t.Cleanup(func() { _ = offline.Close() })

	assert.True(t, offline.IsStale(), "fallback Resolver must report IsStale")

	// Lookups against the stale snapshot work.
	mt, err := offline.FindMessageByName("billing.Config")
	require.NoError(t, err, "cached descriptors must resolve through the offline Resolver")
	assert.Equal(t, protoreflect.FullName("billing.Config"), mt.Descriptor().FullName())

	// Stale resolver: Refresh returns the sentinel.
	require.ErrorIs(t, offline.Refresh(t.Context()), client.ErrStaleResolver,
		"Refresh on a stale Resolver must return ErrStaleResolver")
}

// TestDiskCache_DialFailsWithoutCache pins the "no cache configured"
// behavior — a dial failure with no WithDiskCache returns the
// underlying error rather than silently doing anything else.
func TestDiskCache_DialFailsWithoutCache(t *testing.T) {
	_, err := client.Dial(context.Background(), "127.0.0.1:1", "anything",
		client.WithRefreshInterval(0),
	)
	require.Error(t, err, "Dial against an unreachable address with no cache must surface the network error")
	assert.NotContains(t, err.Error(), "cache",
		"error should describe the network failure, not the cache (which isn't configured)")
}

// TestDiskCache_FallbackOnMissingCacheReportsBothErrors verifies
// that when both online and cache paths fail, the returned error
// is informative — mentions both the dial failure and the missing
// cache file.
func TestDiskCache_FallbackOnMissingCacheReportsBothErrors(t *testing.T) {
	emptyCache := t.TempDir() // no namespace subdir written
	_, err := client.Dial(context.Background(), "127.0.0.1:1", "no-such-namespace",
		client.WithRefreshInterval(0),
		client.WithDiskCache(emptyCache),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "registry unreachable")
	assert.Contains(t, err.Error(), "cache load failed")
}

// TestDiskCache_AtomicWriteDoesNotLeaveTempFiles is a property check
// for the persistence helper — successful writes shouldn't leave
// .tmp files behind, regardless of how many writes happened.
func TestDiskCache_AtomicWriteDoesNotLeaveTempFiles(t *testing.T) {
	srv := clienttest.Start(t)
	const ns = "atomic-write"
	srv.PublishAndPromote(t, ns, "s1", map[string][]byte{
		"s1.proto": []byte(`syntax = "proto3"; package s1; message A { string a = 1; }`),
	})
	srv.PublishAndPromote(t, ns, "s2", map[string][]byte{
		"s2.proto": []byte(`syntax = "proto3"; package s2; message B { string b = 1; }`),
	})

	cacheDir := t.TempDir()
	r, err := client.New(t.Context(), srv.Conn, ns,
		client.WithRefreshInterval(0),
		client.WithDiskCache(cacheDir),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	// Walk the cache directory; assert no .tmp leftovers.
	require.NoError(t, filepath.Walk(filepath.Join(cacheDir, ns), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		assert.NotContains(t, info.Name(), ".tmp",
			"atomic write should clean up its temp files: found %s", path)
		return nil
	}))
}
