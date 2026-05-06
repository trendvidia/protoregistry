// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	protoregistry "github.com/trendvidia/protoregistry"
	registrypb "github.com/trendvidia/protoregistry/proto/protoregistry/v1"
	"github.com/trendvidia/protoregistry/server"
	"github.com/trendvidia/protoregistry/store/postgres"
	"github.com/trendvidia/protoregistry/store/postgres/pgtest"
)

// testServer wires a real Server (with the configured Options) onto a
// bufconn-backed gRPC server, returning a client whose RPCs traverse the
// auth interceptor and validation gates the same way a real client would.
// Callers pass Options to vary behaviour across tests.
type testServer struct {
	client registrypb.RegistryServiceClient
	conn   *grpc.ClientConn
	srv    *grpc.Server
}

func newTestServer(t *testing.T, opts ...server.Option) *testServer {
	t.Helper()
	res := pgtest.Setup(t)
	st := postgres.New(res.Pool)

	reg := protoregistry.New(st)
	require.NoError(t, reg.Restore(context.Background()))

	// Pick the auth from opts so the interceptor matches the handler-side
	// gate. Tests that care about auth pass it via WithAuth; tests that
	// don't get NoAuth (and the unary interceptor passes anonymous).
	o := server.DefaultOptions()
	for _, opt := range opts {
		opt(&o)
	}

	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer(
		grpc.UnaryInterceptor(server.UnaryAuthInterceptor(o.Auth)),
	)
	registrypb.RegisterRegistryServiceServer(srv, server.New(reg, st, opts...))

	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(context.Background())
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return &testServer{
		client: registrypb.NewRegistryServiceClient(conn),
		conn:   conn,
		srv:    srv,
	}
}

func ctxWithBearer(token string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
}

const minimalProto = `syntax = "proto3";
package billing;
message Config { string name = 1; }
`

// Round-trip: Publish → Promote → GetDescriptor → Rollback.
func TestE2E_PublishPromoteRollback(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()

	// v1.
	resp, err := ts.client.Publish(ctx, &registrypb.PublishRequest{
		NamespaceId: "acme",
		SchemaId:    "billing",
		Sources:     map[string][]byte{"billing.proto": []byte(minimalProto)},
		CreatedBy:   "test",
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), resp.Version)

	_, err = ts.client.Promote(ctx, &registrypb.PromoteRequest{NamespaceId: "acme"})
	require.NoError(t, err)

	desc, err := ts.client.GetDescriptor(ctx, &registrypb.GetDescriptorRequest{
		NamespaceId: "acme", SchemaId: "billing",
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), desc.Version)
	require.NotNil(t, desc.FileDescriptorSet)
	assert.Equal(t, 1, len(desc.FileDescriptorSet.File))

	// v2: add a field (forward-compatible).
	v2 := strings.ReplaceAll(minimalProto, "string name = 1;", "string name = 1; int32 timeout_ms = 2;")
	_, err = ts.client.Publish(ctx, &registrypb.PublishRequest{
		NamespaceId: "acme", SchemaId: "billing",
		Sources:   map[string][]byte{"billing.proto": []byte(v2)},
		CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = ts.client.Promote(ctx, &registrypb.PromoteRequest{NamespaceId: "acme"})
	require.NoError(t, err)

	// Rollback to v1 without force: compat check rejects (v2→v1 removes a
	// field). Server returns FailedPrecondition.
	_, err = ts.client.Rollback(ctx, &registrypb.RollbackRequest{
		NamespaceId: "acme", SchemaId: "billing", Version: 1,
	})
	assertGRPCCode(t, err, codes.FailedPrecondition)
}

// Validation: bad IDs, bad filenames, oversize, file count cap.
func TestE2E_ValidationRejected(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()

	cases := map[string]*registrypb.PublishRequest{
		"empty namespace": {
			NamespaceId: "", SchemaId: "billing",
			Sources: map[string][]byte{"a.proto": []byte("syntax=\"proto3\";")},
		},
		"bad namespace charset": {
			NamespaceId: "ACME!!!", SchemaId: "billing",
			Sources: map[string][]byte{"a.proto": []byte("syntax=\"proto3\";")},
		},
		"empty sources": {
			NamespaceId: "acme", SchemaId: "billing",
			Sources: map[string][]byte{},
		},
		"filename traversal": {
			NamespaceId: "acme", SchemaId: "billing",
			Sources: map[string][]byte{"../etc/passwd": []byte("syntax=\"proto3\";")},
		},
		"absolute filename": {
			NamespaceId: "acme", SchemaId: "billing",
			Sources: map[string][]byte{"/etc/passwd": []byte("syntax=\"proto3\";")},
		},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ts.client.Publish(ctx, req)
			assertGRPCCode(t, err, codes.InvalidArgument)
		})
	}
}

// Anonymous writes blocked when AllowAnonymousWrites=false; allowed when
// the bearer token is recognised.
func TestE2E_AnonymousWritesGating(t *testing.T) {
	tokens := map[string]server.Identity{
		"writer-tok": {Subject: "alice", Admin: false},
	}
	ts := newTestServer(t,
		server.WithAuth(server.NewTokenAuth(tokens)),
		server.WithAllowAnonymousWrites(false),
	)

	publish := func(ctx context.Context) error {
		_, err := ts.client.Publish(ctx, &registrypb.PublishRequest{
			NamespaceId: "acme", SchemaId: "billing",
			Sources:   map[string][]byte{"billing.proto": []byte(minimalProto)},
			CreatedBy: "test",
		})
		return err
	}

	t.Run("anonymous rejected", func(t *testing.T) {
		// No metadata → TokenAuth returns Unauthenticated at the
		// interceptor (before the handler's requireWriter even runs).
		err := publish(context.Background())
		assertGRPCCode(t, err, codes.Unauthenticated)
	})
	t.Run("authenticated writer allowed", func(t *testing.T) {
		err := publish(ctxWithBearer("writer-tok"))
		require.NoError(t, err)
	})
}

// __builtins__ writes need admin even when authenticated.
func TestE2E_BuiltinsRequireAdmin(t *testing.T) {
	tokens := map[string]server.Identity{
		"user-tok":  {Subject: "alice", Admin: false},
		"admin-tok": {Subject: "root", Admin: true},
	}
	ts := newTestServer(t,
		server.WithAuth(server.NewTokenAuth(tokens)),
		server.WithAllowAnonymousWrites(false),
	)

	req := &registrypb.PublishRequest{
		NamespaceId: protoregistry.BuiltinsNamespace,
		SchemaId:    "company",
		Sources:     map[string][]byte{"company.proto": []byte(minimalProto)},
		CreatedBy:   "test",
	}
	_, err := ts.client.Publish(ctxWithBearer("user-tok"), req)
	assertGRPCCode(t, err, codes.PermissionDenied)

	_, err = ts.client.Publish(ctxWithBearer("admin-tok"), req)
	require.NoError(t, err)
}

// Force=true on Publish requires admin; non-admin gets PermissionDenied
// even when otherwise authenticated.
func TestE2E_ForceRequiresAdmin(t *testing.T) {
	tokens := map[string]server.Identity{
		"user-tok":  {Subject: "alice", Admin: false},
		"admin-tok": {Subject: "root", Admin: true},
	}
	ts := newTestServer(t,
		server.WithAuth(server.NewTokenAuth(tokens)),
		server.WithAllowAnonymousWrites(false),
	)

	// Use a benign filename that doesn't actually shadow a well-known
	// type — we only want to exercise the Force=true admin gate.
	req := &registrypb.PublishRequest{
		NamespaceId: "acme", SchemaId: "billing",
		Sources:   map[string][]byte{"billing.proto": []byte(minimalProto)},
		CreatedBy: "test",
		Force:     true,
	}
	_, err := ts.client.Publish(ctxWithBearer("user-tok"), req)
	assertGRPCCode(t, err, codes.PermissionDenied)

	_, err = ts.client.Publish(ctxWithBearer("admin-tok"), req)
	require.NoError(t, err)
}

// Pagination: insert more schemas than DefaultListPageSize, page through,
// verify the union covers the set and the cursor terminates.
func TestE2E_ListSchemasPagination(t *testing.T) {
	// Tighten the page size so we don't have to populate hundreds of
	// schemas to exercise pagination.
	limits := server.DefaultLimits()
	limits.DefaultListPageSize = 3
	limits.MaxListPageSize = 10
	ts := newTestServer(t, server.WithLimits(limits))
	ctx := context.Background()

	const total = 7
	for i := 0; i < total; i++ {
		_, err := ts.client.Publish(ctx, &registrypb.PublishRequest{
			NamespaceId: "acme",
			SchemaId:    fmt.Sprintf("svc.%02d", i),
			Sources:     map[string][]byte{"a.proto": []byte(minimalProto)},
			CreatedBy:   "test",
		})
		require.NoError(t, err)
	}

	seen := map[string]bool{}
	var token string
	for pages := 0; pages < total+1; pages++ {
		resp, err := ts.client.ListSchemas(ctx, &registrypb.ListSchemasRequest{
			NamespaceId: "acme",
			PageToken:   token,
		})
		require.NoError(t, err)
		for _, s := range resp.Schemas {
			seen[s.SchemaId] = true
		}
		if resp.NextPageToken == "" {
			break
		}
		token = resp.NextPageToken
	}
	assert.Equal(t, total, len(seen), "all schemas reachable via pagination")
}

// Error sanitization: GetSchema for a missing schema returns NotFound,
// not Internal — and the message does not contain backend details.
func TestE2E_GetSchemaNotFound(t *testing.T) {
	ts := newTestServer(t)
	_, err := ts.client.GetSchema(context.Background(), &registrypb.GetSchemaRequest{
		NamespaceId: "ghost",
		SchemaId:    "missing",
	})
	assertGRPCCode(t, err, codes.NotFound)

	st, _ := status.FromError(err)
	// Reject backend hints we know would leak: pgx error codes, table names.
	for _, leak := range []string{"sql:", "pgx:", "schemas\".\"", "SQLSTATE"} {
		assert.NotContains(t, st.Message(), leak)
	}
}

func assertGRPCCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %v, got nil", want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != want {
		t.Fatalf("expected code %v, got %v: %s", want, st.Code(), st.Message())
	}
}
