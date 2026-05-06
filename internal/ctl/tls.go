// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package ctl

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// clientAuthRequireAndVerify is re-exported from crypto/tls so the call
// site in serve.go does not need a transitive import on the constant.
const clientAuthRequireAndVerify = tls.RequireAndVerifyClientCert

func loadX509KeyPair(certFile, keyFile string) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("loading TLS keypair: %w", err)
	}
	return cert, nil
}

func newTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
}

func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path) // #nosec G304 -- CLI-supplied CA bundle path
	if err != nil {
		return nil, fmt.Errorf("reading CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("CA file did not contain any valid PEM certificates")
	}
	return pool, nil
}
