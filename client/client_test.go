// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package client_test

import (
	"context"
	"crypto/tls"
	"log"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/trendvidia/protoregistry/client"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// fakeConn is a non-nil *grpc.ClientConn whose only purpose is to slip
// past the nil-check in New so we can exercise downstream validation.
// Tests that touch it must error out before any RPC is attempted.
var fakeConn grpc.ClientConn

// Compile-time interface satisfaction: confirms the public API slots
// into protobuf-go's standard resolver interfaces.
var (
	_ protoregistry.MessageTypeResolver   = (*client.Resolver)(nil)
	_ protoregistry.ExtensionTypeResolver = (*client.Resolver)(nil)
	_ protodesc.Resolver                  = (*client.Resolver)(nil)
)

func TestNew_NilConn(t *testing.T) {
	_, err := client.New(context.Background(), nil, "examples")
	if err == nil || !strings.Contains(err.Error(), "nil grpc.ClientConn") {
		t.Fatalf("New(nil conn): got %v, want nil-conn error", err)
	}
}

func TestNew_EmptyNamespace(t *testing.T) {
	// Use a dummy non-nil pointer; the namespace check happens before
	// the conn is touched.
	_, err := client.New(context.Background(), &fakeConn, "")
	if err == nil || !strings.Contains(err.Error(), "empty namespace") {
		t.Fatalf("New(empty namespace): got %v, want empty-namespace error", err)
	}
}

// TestWithTransportCredentials_OptionIsAccepted is a smoke test for
// the new Dial-side TLS option. It doesn't stand up a TLS server —
// the integration suite (TestIntegration) covers end-to-end Dial
// against a real backend with insecure creds. This test just guards
// against regressions where the option's signature drifts away from
// the credentials.TransportCredentials interface or the option fails
// to type-check against the public Option type.
func TestWithTransportCredentials_OptionIsAccepted(t *testing.T) {
	tlsCreds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	opt := client.WithTransportCredentials(tlsCreds)
	if opt == nil {
		t.Fatal("WithTransportCredentials returned a nil Option")
	}
	// The option being a *client.Option (a function type) means we
	// can't observe its effect without dialing — but the type
	// alignment alone is the public contract callers depend on.
	acceptOption(opt)
}

// acceptOption is a no-op helper whose parameter type pins the
// return type of WithTransportCredentials at compile time.
func acceptOption(_ client.Option) {
}

func TestPin_Empty(t *testing.T) {
	r := &client.Resolver{}
	_, err := r.Pin(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "empty pin map") {
		t.Fatalf("Pin(nil): got %v, want empty-pin-map error", err)
	}
}

func TestSchemaResolver_NilParent(t *testing.T) {
	var s *client.SchemaResolver
	if got := s.SchemaID(); got != "" {
		t.Fatalf("nil SchemaResolver.SchemaID: got %q, want empty", got)
	}
	if _, err := s.FindMessageByName("anything"); err == nil {
		t.Fatalf("nil SchemaResolver.FindMessageByName: got nil error")
	}
}

// Example demonstrates the canonical wiring: Dial a registry, fetch a
// message descriptor by fully-qualified name, and decode a PXF payload
// against it via protowire-go.
//
// The example compiles but is not executed (no // Output: directive),
// since it dials a server that is not running here. It serves as a
// godoc-rendered, vet-checked source of truth for the API shape.
func Example() {
	var pxfBytes []byte // payload produced elsewhere

	ctx := context.Background()
	r, err := client.Dial(ctx, "registry.internal:50051", "billing")
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = r.Close() }()

	desc, err := r.FindDescriptorByName("billing.v1.Config")
	if err != nil {
		log.Print(err)
		return
	}

	msg, err := pxf.UnmarshalDescriptor(pxfBytes, desc.(protoreflect.MessageDescriptor))
	if err != nil {
		log.Print(err)
		return
	}
	_ = msg
}

// ExampleResolver_Schema shows the SchemaResolver path, useful when the
// caller already knows which schema in the namespace owns the type.
// It's cheaper (skips the cross-schema name index) and immune to
// collisions across schemas.
func ExampleResolver_Schema() {
	ctx := context.Background()
	r, err := client.Dial(ctx, "registry.internal:50051", "billing")
	if err != nil {
		log.Print(err)
		return
	}
	defer func() { _ = r.Close() }()

	configSchema := r.Schema("config")
	msg, err := configSchema.NewMessage("billing.v1.Config")
	if err != nil {
		log.Print(err)
		return
	}
	_ = msg
}
