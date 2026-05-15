// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trendvidia/protoregistry/authz"
)

func TestAllowAll_PermitsEverything(t *testing.T) {
	ctx := context.Background()
	a := authz.AllowAll{}

	require.NoError(t, a.CanCreateNamespace(ctx, "ns", nil))
	parent := "parent"
	require.NoError(t, a.CanCreateNamespace(ctx, "ns", &parent))
	require.NoError(t, a.CanSetNamespaceParent(ctx, "ns", &parent))
	require.NoError(t, a.CanSetNamespaceParent(ctx, "ns", nil))
	require.NoError(t, a.CanPublish(ctx, "ns", "schema"))
	require.NoError(t, a.CanPromote(ctx, "ns"))
	require.NoError(t, a.CanRebase(ctx, "ns", "snap1"))
}

// onlyAdmin is a sample Authorizer used to exercise the deny path. It
// permits operations only when the context carries actorKey == "admin".
type actorKey struct{}
type onlyAdmin struct{}

func (onlyAdmin) check(ctx context.Context) error {
	if v, _ := ctx.Value(actorKey{}).(string); v == "admin" {
		return nil
	}
	return authz.ErrPermissionDenied
}
func (a onlyAdmin) CanCreateNamespace(ctx context.Context, _ string, _ *string) error {
	return a.check(ctx)
}
func (a onlyAdmin) CanSetNamespaceParent(ctx context.Context, _ string, _ *string) error {
	return a.check(ctx)
}
func (a onlyAdmin) CanPublish(ctx context.Context, _, _ string) error { return a.check(ctx) }
func (a onlyAdmin) CanPromote(ctx context.Context, _ string) error    { return a.check(ctx) }
func (a onlyAdmin) CanRebase(ctx context.Context, _, _ string) error  { return a.check(ctx) }

func TestCustomAuthorizer_DeniesNonAdmin(t *testing.T) {
	a := onlyAdmin{}

	ctxAnon := context.Background()
	err := a.CanPublish(ctxAnon, "ns", "schema")
	require.Error(t, err)
	assert.True(t, errors.Is(err, authz.ErrPermissionDenied),
		"deny errors must wrap ErrPermissionDenied so callers can detect them")
}

func TestCustomAuthorizer_PermitsAdmin(t *testing.T) {
	a := onlyAdmin{}
	ctxAdmin := context.WithValue(context.Background(), actorKey{}, "admin")

	require.NoError(t, a.CanPublish(ctxAdmin, "ns", "schema"))
	require.NoError(t, a.CanSetNamespaceParent(ctxAdmin, "ns", nil))
}

// Compile-time check: AllowAll must satisfy the Authorizer interface.
var _ authz.Authorizer = authz.AllowAll{}
