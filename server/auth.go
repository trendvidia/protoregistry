// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Identity is the authenticated principal behind an RPC. It is attached to
// the request context by the auth interceptor; handlers retrieve it via
// IdentityFromContext.
//
// Subject is the human/service name (used in audit logs). Admin grants the
// bearer the ability to perform privileged operations: writes to the
// __builtins__ namespace, Publish with Force=true, and (in a future patch)
// rollbacks that bypass dependent compat checks.
type Identity struct {
	Subject string
	Admin   bool
}

// AnonymousIdentity is the placeholder returned by NoAuth and used when no
// authentication metadata accompanies a request.
var AnonymousIdentity = Identity{Subject: "anonymous", Admin: false}

type identityCtxKey struct{}

// IdentityFromContext returns the Identity attached by the auth
// interceptor, or AnonymousIdentity if none is present.
func IdentityFromContext(ctx context.Context) Identity {
	if v, ok := ctx.Value(identityCtxKey{}).(Identity); ok {
		return v
	}
	return AnonymousIdentity
}

// withIdentity is used by the interceptor to attach the result of
// Authenticator.Authenticate to the request context.
func withIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// Authenticator extracts an Identity from incoming RPC metadata.
// Implementations may consult bearer tokens, mTLS client certs, JWT
// claims, or any other transport-supplied data.
//
// Returning a non-nil error rejects the RPC; the error should be a gRPC
// status (e.g. codes.Unauthenticated) so the caller sees a meaningful
// failure rather than codes.Internal.
type Authenticator interface {
	Authenticate(ctx context.Context, fullMethod string) (Identity, error)
}

// NoAuth permits every request, returning AnonymousIdentity. It is the
// default when the server is constructed without WithAuth and is intended
// for local development and trusted-network deployments. Combined with
// AllowAnonymousWrites=false it allows reads but rejects writes; combined
// with AllowAnonymousWrites=true (the back-compat default) it preserves the
// pre-auth-seam behaviour and emits a startup warning instead.
type NoAuth struct{}

// Authenticate always succeeds with AnonymousIdentity.
func (NoAuth) Authenticate(_ context.Context, _ string) (Identity, error) {
	return AnonymousIdentity, nil
}

// TokenAuth authenticates bearer tokens against a static map loaded from a
// file or constructed in code. Tokens are presented in the standard
// `authorization: Bearer <token>` metadata header.
//
// File format (one entry per line, tab-separated; `#` starts a comment):
//
//	# token        subject  role
//	abc123def456   alice    admin
//	0123456789ab   ci-bot   user
//
// `role` is `admin` or `user` (default `user`). Tokens with role `admin`
// receive Identity.Admin = true.
type TokenAuth struct {
	tokens map[string]Identity
}

// NewTokenAuth constructs a TokenAuth over the given token→identity map.
// Callers wanting a file loader should use ParseTokenFile.
func NewTokenAuth(tokens map[string]Identity) *TokenAuth {
	cp := make(map[string]Identity, len(tokens))
	for k, v := range tokens {
		cp[k] = v
	}
	return &TokenAuth{tokens: cp}
}

// Authenticate looks up the bearer token in the configured map. Missing,
// malformed, or unknown tokens produce codes.Unauthenticated.
func (t *TokenAuth) Authenticate(ctx context.Context, _ string) (Identity, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return Identity{}, status.Error(codes.Unauthenticated, "missing metadata")
	}
	values := md.Get("authorization")
	if len(values) == 0 {
		return Identity{}, status.Error(codes.Unauthenticated, "missing authorization header")
	}
	const prefix = "Bearer "
	authz := values[0]
	if !strings.HasPrefix(authz, prefix) {
		return Identity{}, status.Error(codes.Unauthenticated, "authorization must use Bearer scheme")
	}
	token := strings.TrimPrefix(authz, prefix)
	id, ok := t.tokens[token]
	if !ok {
		return Identity{}, status.Error(codes.Unauthenticated, "unknown token")
	}
	return id, nil
}

// ParseTokenFile reads the TokenAuth file format described on the
// TokenAuth type. It returns the parsed token map suitable for
// NewTokenAuth.
func ParseTokenFile(r io.Reader) (map[string]Identity, error) {
	tokens := make(map[string]Identity)
	scanner := bufio.NewScanner(r)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("line %d: expected at least <token> <subject>", lineNum)
		}
		token, subject := fields[0], fields[1]
		role := "user"
		if len(fields) >= 3 {
			role = fields[2]
		}
		switch role {
		case "user", "admin":
		default:
			return nil, fmt.Errorf("line %d: role must be 'user' or 'admin', got %q", lineNum, role)
		}
		if _, dup := tokens[token]; dup {
			return nil, fmt.Errorf("line %d: duplicate token", lineNum)
		}
		tokens[token] = Identity{Subject: subject, Admin: role == "admin"}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading token file: %w", err)
	}
	return tokens, nil
}

// UnaryAuthInterceptor returns a grpc.UnaryServerInterceptor that runs the
// configured Authenticator and stashes the resulting Identity on the
// request context. Handlers retrieve it via IdentityFromContext.
func UnaryAuthInterceptor(a Authenticator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		id, err := a.Authenticate(ctx, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(withIdentity(ctx, id), req)
	}
}
