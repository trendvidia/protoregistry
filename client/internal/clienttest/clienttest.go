// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package clienttest is a test-only harness that wires a real
// protoregistry server (Postgres + gRPC over bufconn) and exposes a
// *grpc.ClientConn ready to be passed to client.New, plus helpers for
// the publish/promote dance.
//
// The harness aborts the test (t.Fatalf) if Docker / Postgres cannot be
// brought up, matching the convention in the rest of the repo. Spinning
// up Postgres takes ~5–10s per test; consolidate scenarios into
// subtests under a single Start to share the container.
package clienttest

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	protoregistry "github.com/trendvidia/protoregistry"
	registrypb "github.com/trendvidia/protoregistry/proto/protoregistry/v1"
	"github.com/trendvidia/protoregistry/server"
	"github.com/trendvidia/protoregistry/store/postgres"
	"github.com/trendvidia/protoregistry/store/postgres/pgtest"
)

// Server bundles a running protoregistry instance and a connected
// gRPC client conn. Pass Conn to client.New; use the helpers below
// to drive the server-side state.
type Server struct {
	Conn *grpc.ClientConn
	rpc  registrypb.RegistryServiceClient
}

// Start brings up Postgres in a container, applies migrations, wires
// the registry onto a bufconn-backed gRPC server, and returns a
// connection ready to be passed to client.New. All resources are
// cleaned up via t.Cleanup.
func Start(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()

	res := pgtest.Setup(t)
	st := postgres.New(res.Pool)
	reg := protoregistry.New(st)
	require.NoError(t, reg.Restore(ctx))

	lis := bufconn.Listen(1024 * 1024)
	grpcSrv := grpc.NewServer()
	registrypb.RegisterRegistryServiceServer(grpcSrv, server.New(reg, st))
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(context.Background())
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return &Server{
		Conn: conn,
		rpc:  registrypb.NewRegistryServiceClient(conn),
	}
}

// CreateNamespace ensures the namespace exists. AlreadyExists is
// treated as benign so callers can invoke it idempotently.
func (s *Server) CreateNamespace(t *testing.T, namespace string) {
	t.Helper()
	_, err := s.rpc.CreateNamespace(context.Background(), &registrypb.CreateNamespaceRequest{
		Id: namespace,
	})
	if err != nil {
		t.Logf("create_namespace %q: %v (treating as benign)", namespace, err)
	}
}

// Publish stages a new version of a schema, returning the version
// number assigned by the server.
func (s *Server) Publish(t *testing.T, namespace, schemaID string, sources map[string][]byte) uint64 {
	t.Helper()
	resp, err := s.rpc.Publish(context.Background(), &registrypb.PublishRequest{
		NamespaceId: namespace,
		SchemaId:    schemaID,
		Sources:     sources,
		CreatedBy:   "client/internal/clienttest",
	})
	require.NoErrorf(t, err, "Publish %s/%s", namespace, schemaID)
	return resp.Version
}

// Promote promotes all staged schemas in the namespace.
func (s *Server) Promote(t *testing.T, namespace string) {
	t.Helper()
	_, err := s.rpc.Promote(context.Background(), &registrypb.PromoteRequest{
		NamespaceId: namespace,
	})
	require.NoErrorf(t, err, "Promote %s", namespace)
}

// PublishAndPromote is the common test setup: ensure the namespace
// exists, publish, promote. Returns the published version.
func (s *Server) PublishAndPromote(t *testing.T, namespace, schemaID string, sources map[string][]byte) uint64 {
	t.Helper()
	s.CreateNamespace(t, namespace)
	v := s.Publish(t, namespace, schemaID, sources)
	s.Promote(t, namespace)
	return v
}

// SetNamespaceParent links a namespace to a parent for chained
// resolution. Both namespaces must already exist.
func (s *Server) SetNamespaceParent(t *testing.T, namespace, parent string) {
	t.Helper()
	_, err := s.rpc.SetNamespaceParent(context.Background(), &registrypb.SetNamespaceParentRequest{
		NamespaceId:       namespace,
		ParentNamespaceId: &parent,
	})
	require.NoErrorf(t, err, "SetNamespaceParent %s → %s", namespace, parent)
}
