// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package client provides a remote-backed protobuf descriptor resolver
// that fetches schemas from a running protoregistry server over gRPC.
//
// A Resolver is bound to a single namespace and implements the standard
// protobuf-go reflection interfaces ([protoregistry.MessageTypeResolver],
// [protoregistry.ExtensionTypeResolver], [protodesc.Resolver]) so it
// composes with anything that takes a resolver: dynamicpb, protojson,
// anypb, and external codec libraries such as
// github.com/trendvidia/protowire-go's PXF and SBE encoders.
//
// # Concrete defaults
//
//   - Eager population. New / Dial fetch every schema in the namespace
//     up front. Lookup misses surface at startup, not in the request path.
//   - Polling refresh. A background goroutine calls ListSchemas on a
//     fixed interval (default 30s) and re-fetches descriptors only for
//     schemas whose current version advanced. Hot-swaps are atomic;
//     readers in flight see a consistent snapshot. Failures during
//     refresh are logged and survived — callers see stale-but-consistent
//     state until the next successful tick.
//   - Fail-loud collisions. If two schemas in the namespace export the
//     same fully-qualified type name, New returns an error rather than
//     silently picking one.
//
// These choices mirror the in-process [resolve.Resolver] semantics where
// possible. Streaming refresh, lazy population, and other strategies are
// out of scope for v0.
//
// # Example
//
// Dial a registry, fetch a message descriptor by fully-qualified name,
// and use it to decode a PXF payload via protowire-go:
//
//	ctx := context.Background()
//	r, err := client.Dial(ctx, "registry.internal:50051", "billing")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer r.Close()
//
//	desc, err := r.FindDescriptorByName("billing.v1.Config")
//	if err != nil {
//	    log.Fatal(err)
//	}
//
//	msg, err := pxf.UnmarshalDescriptor(pxfBytes, desc.(protoreflect.MessageDescriptor))
//	if err != nil {
//	    log.Fatal(err)
//	}
//	_ = msg
//
// The Resolver also drops into protojson and anypb without adapter code:
//
//	opts := protojson.UnmarshalOptions{Resolver: r}
//	err := opts.Unmarshal(jsonBytes, msg)
//
// [resolve.Resolver]: https://pkg.go.dev/github.com/trendvidia/protoregistry/resolve#Resolver
package client
