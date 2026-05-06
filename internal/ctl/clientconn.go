// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package ctl

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// clientConnConfig captures the transport-and-auth settings used to dial
// the server from any client subcommand. The zero value dials in plaintext
// (back-compat with pre-auth-seam deployments). Set tls=true (or any of
// the tls* fields) to enable TLS; set token to attach bearer auth.
type clientConnConfig struct {
	serverAddr    string
	tls           bool
	tlsCA         string
	tlsCert       string
	tlsKey        string
	tlsServerName string
	tlsSkipVerify bool
	token         string
}

// tlsEnabled reports whether any TLS-related flag was set. We treat
// passing --tls-ca / --tls-cert / --tls-server-name / --tls-skip-verify
// as implicitly enabling TLS so the user does not have to pass --tls
// alongside them.
func (c clientConnConfig) tlsEnabled() bool {
	return c.tls ||
		c.tlsCA != "" ||
		c.tlsCert != "" ||
		c.tlsKey != "" ||
		c.tlsServerName != "" ||
		c.tlsSkipVerify
}

// dialServer opens a *grpc.ClientConn against cfg.serverAddr with the
// configured transport credentials and (optional) bearer token.
func dialServer(cfg clientConnConfig) (*grpc.ClientConn, error) {
	dialOpts := make([]grpc.DialOption, 0, 2)

	if cfg.tlsEnabled() {
		tlsCfg, err := buildClientTLSConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("building TLS config: %w", err)
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	if cfg.token != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(bearerCreds{token: cfg.token}))
	}

	conn, err := grpc.NewClient(cfg.serverAddr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", cfg.serverAddr, err)
	}
	return conn, nil
}

// buildClientTLSConfig assembles a *tls.Config from the TLS-related
// flags. Empty fields are left at the Go defaults — passing nothing but
// --tls yields a config that uses the system root pool with hostname
// verification, which is what most users want.
func buildClientTLSConfig(cfg clientConnConfig) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: cfg.tlsServerName,
		// #nosec G402 — gated behind explicit --tls-skip-verify flag so
		// the user opts in knowingly. Documented as testing-only.
		InsecureSkipVerify: cfg.tlsSkipVerify,
	}

	if cfg.tlsCA != "" {
		pool, err := loadCAPool(cfg.tlsCA)
		if err != nil {
			return nil, fmt.Errorf("loading server CA: %w", err)
		}
		tlsCfg.RootCAs = pool
	}

	if cfg.tlsCert != "" || cfg.tlsKey != "" {
		if cfg.tlsCert == "" || cfg.tlsKey == "" {
			return nil, errors.New("--tls-cert and --tls-key must be specified together")
		}
		cert, err := loadX509KeyPair(cfg.tlsCert, cfg.tlsKey)
		if err != nil {
			return nil, fmt.Errorf("loading client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	return tlsCfg, nil
}

// bearerCreds attaches an `authorization: Bearer <token>` header to every
// outgoing RPC. RequireTransportSecurity returns false to preserve the
// option of running on a trusted network without TLS; operators who want
// belt-and-suspenders should pair --token with --tls.
type bearerCreds struct{ token string }

func (b bearerCreds) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (bearerCreds) RequireTransportSecurity() bool { return false }
