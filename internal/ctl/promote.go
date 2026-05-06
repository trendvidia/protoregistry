// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package ctl

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	registrypb "github.com/trendvidia/protoregistry/proto/protoregistry/v1"
)

func newPromoteCmd(cli *CLI, defaultNS *string) *cobra.Command {
	return &cobra.Command{
		Use:   "promote [namespace]",
		Short: "Promote all staged versions to current",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, err := resolveNamespace(args, 0, defaultNS)
			if err != nil {
				return err
			}

			resp, err := cli.client.Promote(cmd.Context(), &registrypb.PromoteRequest{
				NamespaceId: ns,
			})
			if err != nil {
				return fmt.Errorf("promote failed: %w", err)
			}

			if len(resp.Promoted) == 0 {
				fmt.Println("Nothing to promote.")
				return nil
			}

			fmt.Printf("Promoted %d schema(s):\n", len(resp.Promoted))
			for _, p := range resp.Promoted {
				fmt.Printf("  %s → v%d\n", p.SchemaId, p.CurrentVersion)
			}
			return nil
		},
	}
}

func newDiscardCmd(cli *CLI, defaultNS *string) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "discard [namespace]",
		Short: "Discard all staged versions in a namespace",
		Long: `Discard all staged versions in a namespace.

This operation is destructive: every Publish that has not yet been Promoted
in the target namespace is dropped. The command prompts for confirmation by
default; pass --yes to skip the prompt (required in non-interactive shells).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, err := resolveNamespace(args, 0, defaultNS)
			if err != nil {
				return err
			}

			ok, err := confirm(os.Stdin, stdinIsTTY(), yes,
				fmt.Sprintf("Discard ALL staged changes in namespace %q?", ns))
			if err != nil {
				return err
			}
			if !ok {
				fmt.Println("Aborted.")
				return nil
			}

			_, err = cli.client.DiscardStaging(cmd.Context(), &registrypb.DiscardStagingRequest{
				NamespaceId: ns,
			})
			if err != nil {
				return fmt.Errorf("discard failed: %w", err)
			}

			fmt.Printf("Discarded all staged changes in namespace %q.\n", ns)
			return nil
		},
	}

	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

func newRollbackCmd(cli *CLI, defaultNS *string) *cobra.Command {
	var (
		promote bool
		yes     bool
		force   bool
	)

	cmd := &cobra.Command{
		Use:   "rollback [namespace] <schema> <version>",
		Short: "Stage a previous version for promotion",
		Long: `Stage a previous version of a schema for promotion.

Rollback is destructive in the sense that it overwrites any currently staged
version of the target schema and (when --promote is passed) replaces the
current version. The command prompts for confirmation by default; pass --yes
to skip the prompt (required in non-interactive shells).`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			var ns, schemaID string
			var versionStr string

			if len(args) == 3 {
				ns, schemaID, versionStr = args[0], args[1], args[2]
			} else {
				var err error
				ns, err = resolveNamespace(nil, 0, defaultNS)
				if err != nil {
					return fmt.Errorf("namespace is required as first argument or via --namespace")
				}
				schemaID, versionStr = args[0], args[1]
			}

			var version uint64
			if _, err := fmt.Sscanf(versionStr, "%d", &version); err != nil {
				return fmt.Errorf("invalid version %q: must be a positive integer", versionStr)
			}

			ok, err := confirm(os.Stdin, stdinIsTTY(), yes,
				fmt.Sprintf("Roll back %s/%s to v%d?", ns, schemaID, version))
			if err != nil {
				return err
			}
			if !ok {
				fmt.Println("Aborted.")
				return nil
			}

			_, err = cli.client.Rollback(cmd.Context(), &registrypb.RollbackRequest{
				NamespaceId: ns,
				SchemaId:    schemaID,
				Version:     version,
				Force:       force,
			})
			if err != nil {
				return fmt.Errorf("rollback failed: %w", err)
			}

			fmt.Printf("Rolled back %s to v%d (staged).\n", schemaID, version)

			if promote {
				resp, err := cli.client.Promote(cmd.Context(), &registrypb.PromoteRequest{
					NamespaceId: ns,
				})
				if err != nil {
					return fmt.Errorf("promote failed: %w", err)
				}
				for _, p := range resp.Promoted {
					fmt.Printf("  Promoted %s → v%d\n", p.SchemaId, p.CurrentVersion)
				}
			} else {
				fmt.Printf("Run 'protoregistry promote %s' to apply.\n", ns)
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&promote, "promote", false, "promote immediately after rollback")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&force, "force", false, "bypass the API-compat check (admin only on the server)")
	return cmd
}
