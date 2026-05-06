// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestNoAuthAlwaysAnonymous(t *testing.T) {
	id, err := NoAuth{}.Authenticate(context.Background(), "/x/y")
	if err != nil {
		t.Fatalf("NoAuth returned error: %v", err)
	}
	if id != AnonymousIdentity {
		t.Fatalf("expected AnonymousIdentity, got %+v", id)
	}
}

func TestIdentityFromContextDefaultsToAnonymous(t *testing.T) {
	if id := IdentityFromContext(context.Background()); id != AnonymousIdentity {
		t.Fatalf("expected anonymous default, got %+v", id)
	}
}

func TestParseTokenFile(t *testing.T) {
	input := `# comment line
# admin token first

abc123	alice	admin
def456	bob	user
nofield	charlie
`
	tokens, err := ParseTokenFile(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(tokens) != 3 {
		t.Fatalf("expected 3 tokens, got %d", len(tokens))
	}
	if !tokens["abc123"].Admin || tokens["abc123"].Subject != "alice" {
		t.Fatalf("alice token wrong: %+v", tokens["abc123"])
	}
	if tokens["def456"].Admin || tokens["def456"].Subject != "bob" {
		t.Fatalf("bob token wrong: %+v", tokens["def456"])
	}
	if tokens["nofield"].Admin || tokens["nofield"].Subject != "charlie" {
		t.Fatalf("charlie token (default user) wrong: %+v", tokens["nofield"])
	}
}

func TestParseTokenFileRejectsBadRole(t *testing.T) {
	if _, err := ParseTokenFile(strings.NewReader("tok sub root\n")); err == nil {
		t.Fatal("expected error for unknown role")
	}
}

func TestParseTokenFileRejectsDuplicate(t *testing.T) {
	input := "tok alice admin\ntok bob user\n"
	if _, err := ParseTokenFile(strings.NewReader(input)); err == nil {
		t.Fatal("expected duplicate token error")
	}
}

func TestTokenAuthAuthenticate(t *testing.T) {
	auth := NewTokenAuth(map[string]Identity{
		"good": {Subject: "alice", Admin: true},
	})

	t.Run("missing metadata", func(t *testing.T) {
		_, err := auth.Authenticate(context.Background(), "/x")
		assertCode(t, err, codes.Unauthenticated)
	})

	t.Run("missing header", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{})
		_, err := auth.Authenticate(ctx, "/x")
		assertCode(t, err, codes.Unauthenticated)
	})

	t.Run("bad scheme", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.MD{"authorization": []string{"Basic abc"}})
		_, err := auth.Authenticate(ctx, "/x")
		assertCode(t, err, codes.Unauthenticated)
	})

	t.Run("unknown token", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.MD{"authorization": []string{"Bearer wrong"}})
		_, err := auth.Authenticate(ctx, "/x")
		assertCode(t, err, codes.Unauthenticated)
	})

	t.Run("good token", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.MD{"authorization": []string{"Bearer good"}})
		id, err := auth.Authenticate(ctx, "/x")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id.Subject != "alice" || !id.Admin {
			t.Fatalf("wrong identity: %+v", id)
		}
	})
}

func assertCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %v, got nil", want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != want {
		t.Fatalf("expected code %v, got %v (%s)", want, st.Code(), st.Message())
	}
}
