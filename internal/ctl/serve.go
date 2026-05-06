// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package ctl

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	protoregistry "github.com/trendvidia/protoregistry"
	"github.com/trendvidia/protoregistry/compiler"
	"github.com/trendvidia/protoregistry/migrations"
	registrypb "github.com/trendvidia/protoregistry/proto/protoregistry/v1"
	"github.com/trendvidia/protoregistry/server"
	"github.com/trendvidia/protoregistry/store/postgres"
)

// serveConfig collects every flag the serve command accepts. Grouping them
// in a struct keeps newServeCmd readable and makes runServe testable
// without re-parsing flags.
type serveConfig struct {
	dbURL                string
	listenAddr           string
	builtInsDir          string
	migrate              bool
	tlsCert              string
	tlsKey               string
	tlsClientCA          string
	authTokensFile       string
	allowAnonymousWrites bool
	maxRecvMsgSize       int
	maxSendMsgSize       int
}

func newServeCmd() *cobra.Command {
	cfg := serveConfig{}

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the gRPC registry server",
		Long: `Start the gRPC registry server.

By default the server accepts anonymous reads AND writes (back-compat with
pre-auth-seam deployments) and emits a startup warning. To lock the server
down, supply --auth-tokens-file and pass --insecure-allow-anonymous-writes=false.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(cmd.Context(), cfg)
		},
	}

	cmd.Flags().StringVar(&cfg.dbURL, "db", "", "PostgreSQL connection URL (env: DATABASE_URL)")
	cmd.Flags().StringVar(&cfg.listenAddr, "listen", ":50051", "gRPC listen address")
	cmd.Flags().StringVar(&cfg.builtInsDir, "builtins", "", "directory of built-in .proto files to bootstrap")
	cmd.Flags().BoolVar(&cfg.migrate, "migrate", false, "run database migrations on startup")

	cmd.Flags().StringVar(&cfg.tlsCert, "tls-cert", "", "PEM-encoded TLS certificate file (enables TLS)")
	cmd.Flags().StringVar(&cfg.tlsKey, "tls-key", "", "PEM-encoded TLS key file")
	cmd.Flags().StringVar(&cfg.tlsClientCA, "tls-client-ca", "", "PEM-encoded CA file for client cert verification (enables mTLS)")

	cmd.Flags().StringVar(&cfg.authTokensFile, "auth-tokens-file", "", "file with bearer tokens (one per line; see SECURITY.md)")
	cmd.Flags().BoolVar(&cfg.allowAnonymousWrites, "insecure-allow-anonymous-writes", true,
		"allow write RPCs from unauthenticated clients (default true for back-compat; set false to require auth)")

	cmd.Flags().IntVar(&cfg.maxRecvMsgSize, "max-recv-msg-size", 64*1024*1024, "gRPC max receive message size in bytes")
	cmd.Flags().IntVar(&cfg.maxSendMsgSize, "max-send-msg-size", 64*1024*1024, "gRPC max send message size in bytes")

	return cmd
}

func runServe(parentCtx context.Context, cfg serveConfig) error {
	logger := slog.Default()

	if cfg.dbURL == "" {
		cfg.dbURL = os.Getenv("DATABASE_URL")
	}
	if cfg.dbURL == "" {
		return fmt.Errorf("--db flag or DATABASE_URL env var is required")
	}

	if cfg.tlsCert != "" && cfg.tlsKey == "" || cfg.tlsCert == "" && cfg.tlsKey != "" {
		return fmt.Errorf("--tls-cert and --tls-key must be specified together")
	}

	ctx, cancel := signal.NotifyContext(parentCtx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Optionally run migrations.
	if cfg.migrate {
		logger.Info("running migrations")
		db, err := sql.Open("pgx", cfg.dbURL)
		if err != nil {
			return fmt.Errorf("opening sql connection for migrations: %w", err)
		}
		defer func() { _ = db.Close() }()

		goose.SetBaseFS(migrations.FS)
		if err := goose.SetDialect("postgres"); err != nil {
			return fmt.Errorf("setting goose dialect: %w", err)
		}
		if err := goose.Up(db, "."); err != nil {
			return fmt.Errorf("running migrations: %w", err)
		}
		logger.Info("migrations complete")
	}

	// Database pool.
	pool, err := pgxpool.New(ctx, cfg.dbURL)
	if err != nil {
		return fmt.Errorf("creating connection pool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pinging database: %w", err)
	}
	logger.Info("connected to database", "url", redactDBURL(cfg.dbURL))

	st := postgres.New(pool)

	// Registry with optional built-ins bootstrap.
	reg := protoregistry.New(st)

	if cfg.builtInsDir != "" {
		logger.Info("bootstrapping built-ins", "dir", cfg.builtInsDir)
		builtins, err := compiler.LoadBuiltIns(cfg.builtInsDir)
		if err != nil {
			return fmt.Errorf("loading built-ins from %s: %w", cfg.builtInsDir, err)
		}
		for _, b := range builtins {
			_, err := reg.Publish(ctx, &protoregistry.PublishRequest{
				NamespaceID: protoregistry.BuiltinsNamespace,
				SchemaID:    "builtins",
				Sources:     map[string][]byte{b.Filename: b.Source},
				CreatedBy:   "bootstrap",
			})
			if err != nil {
				return fmt.Errorf("publishing built-in %s: %w", b.Filename, err)
			}
		}
		if _, err := reg.Promote(ctx, protoregistry.BuiltinsNamespace); err != nil {
			return fmt.Errorf("promoting built-ins: %w", err)
		}
		logger.Info("built-ins bootstrapped", "files", len(builtins))
	}

	// Restore in-memory state from database.
	logger.Info("restoring registry state")
	if err := reg.Restore(ctx); err != nil {
		return fmt.Errorf("restoring registry: %w", err)
	}
	logger.Info("restore complete")

	// Build the gRPC server with the configured authenticator, message
	// caps, keepalive, and (optional) TLS. The authenticator runs as a
	// unary interceptor and stashes the resolved Identity on the request
	// context — server.requireWriter / requireAdmin consume it.
	auth, err := buildAuthenticator(cfg, logger)
	if err != nil {
		return fmt.Errorf("building authenticator: %w", err)
	}
	if _, ok := auth.(server.NoAuth); ok && cfg.allowAnonymousWrites {
		logger.Warn("server is unauthenticated and accepts anonymous writes; " +
			"set --auth-tokens-file and --insecure-allow-anonymous-writes=false to lock down")
	}

	grpcOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(cfg.maxRecvMsgSize),
		grpc.MaxSendMsgSize(cfg.maxSendMsgSize),
		grpc.UnaryInterceptor(server.UnaryAuthInterceptor(auth)),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             15 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
			Time:              30 * time.Second,
			Timeout:           20 * time.Second,
		}),
	}
	if cfg.tlsCert != "" {
		creds, err := buildTLSCredentials(cfg)
		if err != nil {
			return fmt.Errorf("building TLS credentials: %w", err)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(creds))
		logger.Info("TLS enabled", "client_ca", cfg.tlsClientCA != "")
	}

	grpcServer := grpc.NewServer(grpcOpts...)
	registrypb.RegisterRegistryServiceServer(grpcServer,
		server.New(reg, st,
			server.WithAuth(auth),
			server.WithLogger(logger),
			server.WithAllowAnonymousWrites(cfg.allowAnonymousWrites),
		),
	)
	reflection.Register(grpcServer)

	lis, err := net.Listen("tcp", cfg.listenAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.listenAddr, err)
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("gRPC server listening", "addr", cfg.listenAddr)
		errCh <- grpcServer.Serve(lis)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down gracefully")
		grpcServer.GracefulStop()
		return nil
	case err := <-errCh:
		return fmt.Errorf("gRPC server error: %w", err)
	}
}

// buildAuthenticator picks an Authenticator based on the configured flags:
// a TokenAuth when --auth-tokens-file is set, otherwise NoAuth.
func buildAuthenticator(cfg serveConfig, logger *slog.Logger) (server.Authenticator, error) {
	if cfg.authTokensFile == "" {
		return server.NoAuth{}, nil
	}
	f, err := os.Open(cfg.authTokensFile)
	if err != nil {
		return nil, fmt.Errorf("opening auth tokens file: %w", err)
	}
	defer func() { _ = f.Close() }()
	tokens, err := server.ParseTokenFile(f)
	if err != nil {
		return nil, fmt.Errorf("parsing auth tokens file: %w", err)
	}
	if len(tokens) == 0 {
		return nil, errors.New("auth tokens file is empty")
	}
	logger.Info("loaded bearer tokens", "count", len(tokens), "file", cfg.authTokensFile)
	return server.NewTokenAuth(tokens), nil
}

// buildTLSCredentials assembles a credentials.TransportCredentials from the
// cert/key/client-CA flags. With a client CA, mTLS is enforced; without
// one, the server presents its cert but does not verify the client.
func buildTLSCredentials(cfg serveConfig) (credentials.TransportCredentials, error) {
	cert, err := loadX509KeyPair(cfg.tlsCert, cfg.tlsKey)
	if err != nil {
		return nil, err
	}
	tlsCfg := newTLSConfig(cert)
	if cfg.tlsClientCA != "" {
		pool, err := loadCAPool(cfg.tlsClientCA)
		if err != nil {
			return nil, fmt.Errorf("loading client CA: %w", err)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = clientAuthRequireAndVerify
	}
	return credentials.NewTLS(tlsCfg), nil
}

// redactDBURL returns the dbURL with any password component replaced by
// "***", so connection logs never expose credentials. Falls back to the
// raw URL if parsing fails (better to log something than nothing, but
// without revealing secrets we may have parsed wrongly).
func redactDBURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return raw
	}
	u.User = url.UserPassword(u.User.Username(), "***")
	return u.String()
}
