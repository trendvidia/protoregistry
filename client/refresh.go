// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"fmt"
	"time"

	registrypb "github.com/trendvidia/protoregistry/proto/protoregistry/v1"
)

// Refresh forces a freshness check now, outside the regular polling
// cadence. Useful in tests and after a known publish/promote cycle.
//
// Refresh is safe to call concurrently with itself and with the
// background refresh loop — calls are serialized internally. Lookups
// never block on Refresh; they read the snapshot atomically.
//
// On error, the previous snapshot is preserved (stale-while-error).
func (r *Resolver) Refresh(ctx context.Context) error {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

	infos, err := r.listAllSchemas(ctx)
	if err != nil {
		return fmt.Errorf("refresh list_schemas: %w", err)
	}

	cur := r.snapshot.Load()
	next := newSnapshot(len(infos))
	changed := false

	for _, info := range infos {
		if info.CurrentVersion == nil {
			continue
		}
		version := *info.CurrentVersion

		if cur != nil {
			if existing, ok := cur.schemas[info.SchemaId]; ok && existing.version == version {
				next.schemas[info.SchemaId] = existing
				continue
			}
		}

		ss, err := r.fetchSchema(ctx, info.SchemaId, version)
		if err != nil {
			return fmt.Errorf("refresh fetch %s@%d: %w", info.SchemaId, version, err)
		}
		next.schemas[info.SchemaId] = ss
		changed = true
	}

	// Detect schema removals.
	if cur != nil {
		for id := range cur.schemas {
			if _, ok := next.schemas[id]; !ok {
				changed = true
				break
			}
		}
	} else {
		changed = len(next.schemas) > 0
	}

	if !changed && cur != nil {
		return nil
	}

	if err := next.buildNameIndex(); err != nil {
		return err
	}
	if err := next.buildAggregates(); err != nil {
		return err
	}
	r.snapshot.Store(next)
	r.logger.Debug("snapshot refreshed",
		"namespace", r.ns,
		"schemas", len(next.schemas),
	)
	return nil
}

// refreshLoop runs Refresh on the configured interval until the
// context is cancelled. Failures are logged and survived — callers see
// stale-but-consistent snapshots until the next successful tick.
func (r *Resolver) refreshLoop(ctx context.Context) {
	defer r.wg.Done()
	t := time.NewTicker(r.cfg.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Refresh(ctx); err != nil {
				r.logger.Warn("refresh failed; serving stale snapshot",
					"namespace", r.ns,
					"err", err,
				)
			}
		}
	}
}

// listAllSchemas paginates through ListSchemas, returning every schema
// the Resolver tracks (filtered by [WithSchemas] if configured).
// Schemas with no current version are still returned — callers decide
// whether to skip them.
func (r *Resolver) listAllSchemas(ctx context.Context) ([]*registrypb.SchemaInfo, error) {
	var all []*registrypb.SchemaInfo
	page := ""
	for {
		resp, err := r.rpc.ListSchemas(ctx, &registrypb.ListSchemasRequest{
			NamespaceId: r.ns,
			PageToken:   page,
		})
		if err != nil {
			return nil, err
		}
		for _, s := range resp.Schemas {
			if !r.tracksSchema(s.SchemaId) {
				continue
			}
			all = append(all, s)
		}
		if resp.NextPageToken == "" {
			break
		}
		page = resp.NextPageToken
	}
	return all, nil
}
