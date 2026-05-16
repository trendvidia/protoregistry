// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package client

import (
	"context"
	"fmt"
	"sort"
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
//
// # Incremental aggregate updates
//
// Refresh applies the per-schema diff to the namespace-wide aggregate
// in place — UnregisterFile / UnregisterMessage / UnregisterEnum /
// UnregisterExtension for schemas that were removed or replaced, then
// UpdateFile / Update* for schemas that were added or replaced. This
// avoids the O(N) cost of rebuilding the aggregate when only a small
// number of schemas changed.
//
// During the brief window between the aggregate mutation and the
// snapshot.Store call, lookups via [Resolver.FindFileByPath] /
// [Resolver.FindExtensionByNumber] may observe the new state while
// per-schema views (via [SchemaResolver]) still reflect the old. For
// schema-consistent reads, route through SchemaResolver or use [Pin].
func (r *Resolver) Refresh(ctx context.Context) error {
	if r.fromCache {
		return ErrStaleResolver
	}
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

	infos, err := r.listAllSchemas(ctx)
	if err != nil {
		return fmt.Errorf("refresh list_schemas: %w", err)
	}

	cur := r.snapshot.Load()
	next := newSnapshot(len(infos))

	// added: new schemaSnapshots that need to be folded into the aggregate.
	// replaced: pairs (oldSS, newSS) where the schema's version advanced —
	//           the old fingerprint must be removed before the new one is
	//           added. Storing the old SS lets removeFromAggregate use its
	//           captured fingerprint without re-walking descriptors.
	var added []*schemaSnapshot
	type replacement struct {
		old, fresh *schemaSnapshot
	}
	var replaced []replacement

	for _, info := range infos {
		if info.CurrentVersion == nil {
			continue
		}
		version := *info.CurrentVersion

		if cur != nil {
			if existing, ok := cur.schemas[info.SchemaId]; ok {
				if existing.version == version {
					// Unchanged: reuse the schemaSnapshot pointer; the
					// aggregate already has its entries from a prior cycle.
					next.schemas[info.SchemaId] = existing
					continue
				}
				// Version advanced — fetch the new descriptor and queue a
				// replacement diff entry.
				fresh, err := r.fetchSchema(ctx, info.SchemaId, version)
				if err != nil {
					return fmt.Errorf("refresh fetch %s@%d: %w", info.SchemaId, version, err)
				}
				next.schemas[info.SchemaId] = fresh
				replaced = append(replaced, replacement{old: existing, fresh: fresh})
				continue
			}
		}

		// Brand new schema (or first refresh cycle).
		fresh, err := r.fetchSchema(ctx, info.SchemaId, version)
		if err != nil {
			return fmt.Errorf("refresh fetch %s@%d: %w", info.SchemaId, version, err)
		}
		next.schemas[info.SchemaId] = fresh
		added = append(added, fresh)
	}

	// Schemas that were in cur but not in next have been removed
	// server-side (or are explicitly excluded by WithSchemas + a server
	// list change). Their aggregate entries need to come out.
	var removed []*schemaSnapshot
	if cur != nil {
		for id, ss := range cur.schemas {
			if _, ok := next.schemas[id]; !ok {
				removed = append(removed, ss)
			}
		}
	}

	if len(added) == 0 && len(replaced) == 0 && len(removed) == 0 && cur != nil {
		// Nothing changed; skip the name-index rebuild and snapshot swap.
		return nil
	}

	if err := next.buildNameIndex(); err != nil {
		return err
	}

	// Apply the diff to the namespace-wide aggregate. Order:
	//   1. Remove entries for replaced and removed schemas first, so
	//      Update* below cannot collide with stale entries on the same
	//      file path or extension number.
	//   2. Apply added and replaced (new versions of) schemas. Sort the
	//      apply list by schemaID so the last-wins resolution of any
	//      file-path or extension-number conflict between siblings is
	//      reproducible across runs.
	for _, ss := range removed {
		if err := r.removeFromAggregate(ss); err != nil {
			return fmt.Errorf("refresh aggregate remove: %w", err)
		}
	}
	for _, rep := range replaced {
		if err := r.removeFromAggregate(rep.old); err != nil {
			return fmt.Errorf("refresh aggregate replace (remove old): %w", err)
		}
	}

	apply := make([]*schemaSnapshot, 0, len(added)+len(replaced))
	apply = append(apply, added...)
	for _, rep := range replaced {
		apply = append(apply, rep.fresh)
	}
	sort.Slice(apply, func(i, j int) bool { return apply[i].schemaID < apply[j].schemaID })
	for _, ss := range apply {
		if err := r.applyToAggregate(ss); err != nil {
			return fmt.Errorf("refresh aggregate apply: %w", err)
		}
	}

	r.snapshot.Store(next)
	r.logger.Debug("snapshot refreshed",
		"namespace", r.ns,
		"schemas", len(next.schemas),
		"added", len(added),
		"replaced", len(replaced),
		"removed", len(removed),
	)
	// Persistence is best-effort: failures log but don't fail Refresh.
	// The in-memory snapshot is already authoritative, and the next
	// refresh tick will retry the write.
	if r.cfg.cacheDir != "" {
		if perr := r.persist(); perr != nil {
			r.logger.Warn("cache persist failed; in-memory snapshot still authoritative",
				"namespace", r.ns, "err", perr)
		}
	}
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
