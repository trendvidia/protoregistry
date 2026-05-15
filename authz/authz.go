// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package authz defines the authorization seam for protoregistry.
//
// The registry does not implement an identity model. Integrators provide
// an Authorizer that reads the acting principal from context and decides
// whether the operation is permitted. The registry calls into the
// Authorizer at every privileged operation, before any state mutation.
//
// Identity extraction is the deployment's responsibility — typically a
// gRPC unary interceptor or HTTP middleware that resolves a bearer token,
// mTLS cert, or session and stashes the principal in context. The
// Authorizer reads from the same key. The registry never inspects the
// actor itself.
//
// See docs/design/namespace-hierarchy.md for the full design including
// decision D5 (privileged operations gated by a pluggable authorizer).
package authz

import (
	"context"
	"errors"
)

// ErrPermissionDenied is the canonical denial error. Authorizer
// implementations SHOULD return this (or wrap it) so callers can
// distinguish authorization failures from other errors via errors.Is.
var ErrPermissionDenied = errors.New("permission denied")

// Authorizer decides whether the principal carried in ctx may perform a
// given operation. Each method returns nil to permit; any non-nil error
// denies. Errors should wrap ErrPermissionDenied.
//
// The interface is explicit per operation rather than generic Check(action,
// resource) so:
//   - parameters per operation are type-checked at compile time
//   - each method's signature documents exactly what the policy decision
//     has available
//   - integrations can embed a default (e.g. AllowAll) and override only
//     the methods they care about
//
// All write operations on the registry hierarchy are gated. Read
// operations (Get*, List*, Find*) are intentionally not gated by this
// interface in the current design; if needed later, additional methods
// can be added in a backward-compatible way (existing implementations
// embedding AllowAll continue to work without changes only if they
// embed; explicit implementations would need to add the new methods).
type Authorizer interface {
	// CanCreateNamespace gates namespace creation. parentID is non-nil
	// when the request specifies an explicit parent — policy may require
	// the parent be in the actor's organization.
	CanCreateNamespace(ctx context.Context, namespaceID string, parentID *string) error

	// CanSetNamespaceParent gates re-parenting an existing namespace.
	// The highest-privilege operation in the hierarchy — controls
	// fallback resolution for an entire namespace's schemas. See D5.
	CanSetNamespaceParent(ctx context.Context, namespaceID string, newParentID *string) error

	// CanPublish gates publishing a new schema version to a namespace.
	CanPublish(ctx context.Context, namespaceID, schemaID string) error

	// CanPromote gates atomically moving staged versions to current
	// within a namespace.
	CanPromote(ctx context.Context, namespaceID string) error

	// CanRebase gates the Rebase operation (phase 4): re-pin a child
	// against a different parent-namespace snapshot and recompile.
	CanRebase(ctx context.Context, namespaceID, targetParentSnapshotID string) error
}

// AllowAll permits every operation. It is the default Authorizer used
// when none is configured.
//
// Suitable for tests and single-tenant local deployments only. Production
// deployments MUST inject a real Authorizer via registry.WithAuthorizer;
// the registry logs a startup warning when AllowAll is in effect.
type AllowAll struct{}

func (AllowAll) CanCreateNamespace(context.Context, string, *string) error    { return nil }
func (AllowAll) CanSetNamespaceParent(context.Context, string, *string) error { return nil }
func (AllowAll) CanPublish(context.Context, string, string) error             { return nil }
func (AllowAll) CanPromote(context.Context, string) error                     { return nil }
func (AllowAll) CanRebase(context.Context, string, string) error              { return nil }
