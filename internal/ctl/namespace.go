// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package ctl

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	registrypb "github.com/trendvidia/protoregistry/proto/protoregistry/v1"
)

func newNamespaceCmd(cli *CLI, defaultNS *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "namespace",
		Aliases: []string{"ns"},
		Short:   "Manage namespaces",
	}

	cmd.AddCommand(
		newNamespaceListCmd(cli),
		newNamespaceCreateCmd(cli),
	)

	return cmd
}

func newNamespaceListCmd(cli *CLI) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all namespaces",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := cli.client.ListNamespaces(cmd.Context(), &registrypb.ListNamespacesRequest{})
			if err != nil {
				return fmt.Errorf("listing namespaces: %w", err)
			}

			if cli.output == "json" {
				return printJSON(os.Stdout, resp.Namespaces)
			}

			rows := make([][]string, len(resp.Namespaces))
			for i, ns := range resp.Namespaces {
				created := ""
				if ns.CreatedAt != nil {
					created = ns.CreatedAt.AsTime().Format("2006-01-02")
				}
				rows[i] = []string{ns.Id, created}
			}
			printTable([]string{"NAMESPACE", "CREATED"}, rows)
			return nil
		},
	}
}

func newNamespaceCreateCmd(cli *CLI) *cobra.Command {
	var metadata []string

	cmd := &cobra.Command{
		Use:   "create <id>",
		Short: "Create a namespace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			md := parseMetadata(metadata)
			_, err := cli.client.CreateNamespace(cmd.Context(), &registrypb.CreateNamespaceRequest{
				Id:       args[0],
				Metadata: md,
			})
			if err != nil {
				return fmt.Errorf("creating namespace: %w", err)
			}
			fmt.Printf("Namespace %q created.\n", args[0])
			return nil
		},
	}

	cmd.Flags().StringArrayVar(&metadata, "metadata", nil, "metadata key=value pairs")
	return cmd
}

func parseMetadata(pairs []string) map[string]string {
	if len(pairs) == 0 {
		return nil
	}
	md := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, _ := strings.Cut(p, "=")
		md[k] = v
	}
	return md
}
