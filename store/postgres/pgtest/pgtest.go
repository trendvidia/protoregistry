// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package pgtest provides a shared test helper for spinning up a
// PostgreSQL container with migrations applied.
package pgtest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx driver for database/sql (used by goose)
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/trendvidia/protoregistry/migrations"
)

// SetupResult holds the resources from setting up a test database.
type SetupResult struct {
	Pool      *pgxpool.Pool
	ConnStr   string
	Container testcontainers.Container
}

// Setup creates a PostgreSQL container, runs migrations, and returns a
// connection pool. The container is automatically terminated when the
// test completes.
func Setup(t *testing.T) *SetupResult {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("protoregistry_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second)),
	)
	if err != nil {
		t.Fatalf("starting postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("terminating container: %v", err)
		}
	})

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("getting connection string: %v", err)
	}

	// Run migrations using goose with embedded SQL files.
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("opening sql connection: %v", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("setting goose dialect: %v", err)
	}
	if err := goose.Up(db, "."); err != nil {
		t.Fatalf("running migrations: %v", err)
	}

	// Create pgx pool for the store.
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatalf("creating pgx pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return &SetupResult{
		Pool:      pool,
		ConnStr:   connStr,
		Container: container,
	}
}
