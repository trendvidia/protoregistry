// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package ctl

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	registrypb "github.com/trendvidia/protoregistry/proto/protoregistry/v1"
)

func newSchemaCmd(cli *CLI, defaultNS *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Manage schemas",
	}

	cmd.AddCommand(
		newSchemaListCmd(cli, defaultNS),
		newSchemaInfoCmd(cli, defaultNS),
		newSchemaSourceCmd(cli, defaultNS),
		newSchemaDescriptorCmd(cli, defaultNS),
	)

	return cmd
}

func newSchemaListCmd(cli *CLI, defaultNS *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list [namespace]",
		Short: "List schemas in a namespace",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, err := resolveNamespace(args, 0, defaultNS)
			if err != nil {
				return err
			}

			resp, err := cli.client.ListSchemas(cmd.Context(), &registrypb.ListSchemasRequest{
				NamespaceId: ns,
			})
			if err != nil {
				return fmt.Errorf("listing schemas: %w", err)
			}

			if cli.output == "json" {
				return printJSON(os.Stdout, resp.Schemas)
			}

			rows := make([][]string, len(resp.Schemas))
			for i, s := range resp.Schemas {
				current := "-"
				if s.CurrentVersion != nil {
					current = fmt.Sprintf("v%d", *s.CurrentVersion)
				}
				staged := "-"
				if s.StagedVersion != nil {
					staged = fmt.Sprintf("v%d", *s.StagedVersion)
				}
				created := ""
				if s.CreatedAt != nil {
					created = s.CreatedAt.AsTime().Format("2006-01-02")
				}
				rows[i] = []string{s.SchemaId, current, staged, created}
			}
			printTable([]string{"SCHEMA", "CURRENT", "STAGED", "CREATED"}, rows)
			return nil
		},
	}
}

func newSchemaInfoCmd(cli *CLI, defaultNS *string) *cobra.Command {
	return &cobra.Command{
		Use:   "info [namespace] <schema>",
		Short: "Show schema details",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var ns, schemaID string
			if len(args) == 2 {
				ns, schemaID = args[0], args[1]
			} else {
				var err error
				ns, err = resolveNamespace(nil, 0, defaultNS)
				if err != nil {
					return fmt.Errorf("namespace is required as first argument or via --namespace")
				}
				schemaID = args[0]
			}

			resp, err := cli.client.GetSchema(cmd.Context(), &registrypb.GetSchemaRequest{
				NamespaceId: ns,
				SchemaId:    schemaID,
			})
			if err != nil {
				return fmt.Errorf("getting schema: %w", err)
			}

			if cli.output == "json" {
				return printJSON(os.Stdout, resp.Schema)
			}

			s := resp.Schema
			fmt.Printf("Namespace:  %s\n", s.NamespaceId)
			fmt.Printf("Schema:     %s\n", s.SchemaId)
			if s.CurrentVersion != nil {
				fmt.Printf("Current:    v%d\n", *s.CurrentVersion)
			} else {
				fmt.Printf("Current:    -\n")
			}
			if s.StagedVersion != nil {
				fmt.Printf("Staged:     v%d\n", *s.StagedVersion)
			}
			if s.CreatedAt != nil {
				fmt.Printf("Created:    %s\n", s.CreatedAt.AsTime().Format("2006-01-02 15:04:05"))
			}
			if len(s.Metadata) > 0 {
				fmt.Printf("Metadata:   %v\n", s.Metadata)
			}
			if len(s.Versions) > 0 {
				vs := make([]string, len(s.Versions))
				for i, v := range s.Versions {
					vs[i] = fmt.Sprintf("v%d", v)
				}
				fmt.Printf("Versions:   %s\n", strings.Join(vs, ", "))
			}
			return nil
		},
	}
}

func newSchemaSourceCmd(cli *CLI, defaultNS *string) *cobra.Command {
	var version uint64

	cmd := &cobra.Command{
		Use:   "source [namespace] <schema>",
		Short: "Show proto source files for a schema version",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var ns, schemaID string
			if len(args) == 2 {
				ns, schemaID = args[0], args[1]
			} else {
				var err error
				ns, err = resolveNamespace(nil, 0, defaultNS)
				if err != nil {
					return fmt.Errorf("namespace is required as first argument or via --namespace")
				}
				schemaID = args[0]
			}

			resp, err := cli.client.GetSource(cmd.Context(), &registrypb.GetSourceRequest{
				NamespaceId: ns,
				SchemaId:    schemaID,
				Version:     version,
			})
			if err != nil {
				return fmt.Errorf("getting source: %w", err)
			}

			if cli.output == "json" {
				return printJSON(os.Stdout, resp)
			}

			fmt.Printf("Version: v%d\n\n", resp.Version)
			for filename, content := range resp.Sources {
				fmt.Printf("=== %s ===\n%s\n", filename, string(content))
			}
			return nil
		},
	}

	cmd.Flags().Uint64Var(&version, "version", 0, "version number (default: current)")
	return cmd
}

func newSchemaDescriptorCmd(cli *CLI, defaultNS *string) *cobra.Command {
	var (
		version uint64
		outFile string
	)

	cmd := &cobra.Command{
		Use:   "descriptor [namespace] <schema>",
		Short: "Get compiled FileDescriptorSet for a schema version",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var ns, schemaID string
			if len(args) == 2 {
				ns, schemaID = args[0], args[1]
			} else {
				var err error
				ns, err = resolveNamespace(nil, 0, defaultNS)
				if err != nil {
					return fmt.Errorf("namespace is required as first argument or via --namespace")
				}
				schemaID = args[0]
			}

			resp, err := cli.client.GetDescriptor(cmd.Context(), &registrypb.GetDescriptorRequest{
				NamespaceId: ns,
				SchemaId:    schemaID,
				Version:     version,
			})
			if err != nil {
				return fmt.Errorf("getting descriptor: %w", err)
			}

			if outFile != "" {
				data, err := proto.Marshal(resp.FileDescriptorSet)
				if err != nil {
					return fmt.Errorf("marshaling descriptor set: %w", err)
				}
				if err := os.WriteFile(outFile, data, 0o644); err != nil {
					return fmt.Errorf("writing file: %w", err)
				}
				fmt.Printf("Wrote v%d descriptor set to %s\n", resp.Version, outFile)
				return nil
			}

			fmt.Printf("Version: v%d\n", resp.Version)
			fds := resp.FileDescriptorSet
			if fds != nil {
				for _, f := range fds.File {
					fmt.Printf("  %s\n", f.GetName())
				}
			}
			return nil
		},
	}

	cmd.Flags().Uint64Var(&version, "version", 0, "version number (default: current)")
	cmd.Flags().StringVar(&outFile, "out", "", "write binary FileDescriptorSet to file")
	return cmd
}
