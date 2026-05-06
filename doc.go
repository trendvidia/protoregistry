// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package protoregistry provides a multi-namespace protobuf schema registry
// with versioning, staging, and hot-swap capabilities.
//
// It uses protocompile to compile .proto source files at runtime, stores
// versioned schemas in Postgres with content-addressable deduplication,
// and serves compiled descriptors for dynamic message creation and validation.
//
// Each namespace is an isolated scope — proto imports resolve only within
// the same namespace, similar to a chroot. Schemas within a namespace are
// versioned independently, with a staging mechanism for coordinated
// multi-schema promotions.
package protoregistry
