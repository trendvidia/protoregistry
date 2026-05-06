// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package ctl implements the protoregistry CLI commands.
package ctl

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	registrypb "github.com/trendvidia/protoregistry/proto/protoregistry/v1"
)

// CLI holds shared state for all commands.
type CLI struct {
	conn   *grpc.ClientConn
	client registrypb.RegistryServiceClient
	output string
}

// NewRootCmd creates the root cobra command with all subcommands.
func NewRootCmd() *cobra.Command {
	var (
		defaultNS    string
		outputFormat string
		connCfg      clientConnConfig
	)

	cli := &CLI{}

	root := &cobra.Command{
		Use:   "protoregistry",
		Short: "Proto schema registry CLI",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// serve command doesn't need a gRPC connection.
			if cmd.Name() == "serve" || cmd.Name() == "protoregistry" {
				return nil
			}
			if connCfg.serverAddr == "" {
				connCfg.serverAddr = os.Getenv("PROTOREGISTRY_SERVER")
			}
			if connCfg.serverAddr == "" {
				connCfg.serverAddr = "localhost:50051"
			}
			if connCfg.token == "" {
				connCfg.token = os.Getenv("PROTOREGISTRY_TOKEN")
			}
			if connCfg.token != "" && !connCfg.tlsEnabled() {
				fmt.Fprintln(os.Stderr,
					"warning: --token is set without TLS; the bearer token "+
						"will be sent over a plaintext connection. Pass --tls "+
						"(or one of --tls-ca/--tls-cert/--tls-server-name) "+
						"to encrypt.")
			}
			conn, err := dialServer(connCfg)
			if err != nil {
				return err
			}
			cli.conn = conn
			cli.client = registrypb.NewRegistryServiceClient(conn)
			cli.output = outputFormat
			return nil
		},
		PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
			if cli.conn != nil {
				return cli.conn.Close()
			}
			return nil
		},
		SilenceUsage: true,
	}

	root.PersistentFlags().StringVarP(&connCfg.serverAddr, "server", "s", "", "gRPC server address (env: PROTOREGISTRY_SERVER, default: localhost:50051)")
	root.PersistentFlags().StringVarP(&defaultNS, "namespace", "n", "", "default namespace")
	root.PersistentFlags().StringVarP(&outputFormat, "output", "o", "table", "output format: table, json")

	root.PersistentFlags().BoolVar(&connCfg.tls, "tls", false, "enable TLS using the system root CA pool")
	root.PersistentFlags().StringVar(&connCfg.tlsCA, "tls-ca", "", "PEM-encoded CA file to verify the server cert against (implies --tls)")
	root.PersistentFlags().StringVar(&connCfg.tlsCert, "tls-cert", "", "PEM-encoded client certificate for mTLS (implies --tls)")
	root.PersistentFlags().StringVar(&connCfg.tlsKey, "tls-key", "", "PEM-encoded client key for mTLS (implies --tls)")
	root.PersistentFlags().StringVar(&connCfg.tlsServerName, "tls-server-name", "", "override the server name used for cert verification (implies --tls)")
	root.PersistentFlags().BoolVar(&connCfg.tlsSkipVerify, "tls-skip-verify", false, "skip server cert verification (testing only; implies --tls)")
	root.PersistentFlags().StringVar(&connCfg.token, "token", "", "bearer token for authentication (env: PROTOREGISTRY_TOKEN)")

	root.AddCommand(
		newServeCmd(),
		newNamespaceCmd(cli, &defaultNS),
		newSchemaCmd(cli, &defaultNS),
		newPushCmd(cli, &defaultNS),
		newLoadCmd(cli, &defaultNS),
		newPromoteCmd(cli, &defaultNS),
		newDiscardCmd(cli, &defaultNS),
		newRollbackCmd(cli, &defaultNS),
		newPXFCmd(cli, &defaultNS),
	)

	return root
}

// resolveNamespace returns the namespace from args or the default flag.
func resolveNamespace(args []string, idx int, defaultNS *string) (string, error) {
	if idx < len(args) {
		return args[idx], nil
	}
	if defaultNS != nil && *defaultNS != "" {
		return *defaultNS, nil
	}
	return "", fmt.Errorf("namespace is required (pass as argument or use --namespace)")
}
