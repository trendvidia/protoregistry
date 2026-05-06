// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package ctl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	registrypb "github.com/trendvidia/protoregistry/proto/protoregistry/v1"
)

func newPushCmd(cli *CLI, defaultNS *string) *cobra.Command {
	var (
		createdBy string
		metadata  []string
		promote   bool
		force     bool
	)

	cmd := &cobra.Command{
		Use:   "push [namespace] <schema> <path...>",
		Short: "Publish proto files to the registry",
		Long: `Reads .proto files from disk and publishes them as a new schema version.

Paths can be files or directories. Directories are walked recursively
for .proto files. The import path key is computed relative to the
directory root.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Parse args: if --namespace is set, first arg is schema, rest are paths.
			// Otherwise: first is namespace, second is schema, rest are paths.
			var ns, schemaID string
			var paths []string

			if defaultNS != nil && *defaultNS != "" {
				ns = *defaultNS
				schemaID = args[0]
				paths = args[1:]
			} else {
				if len(args) < 3 {
					return fmt.Errorf("usage: push <namespace> <schema> <path...> (or set --namespace)")
				}
				ns = args[0]
				schemaID = args[1]
				paths = args[2:]
			}

			sources, err := resolveProtoSources(paths)
			if err != nil {
				return err
			}
			if len(sources) == 0 {
				return fmt.Errorf("no .proto files found in %v", paths)
			}

			md := parseMetadata(metadata)

			resp, err := cli.client.Publish(cmd.Context(), &registrypb.PublishRequest{
				NamespaceId: ns,
				SchemaId:    schemaID,
				Sources:     sources,
				CreatedBy:   createdBy,
				Metadata:    md,
				Force:       force,
			})
			if err != nil {
				return fmt.Errorf("publish failed: %w", err)
			}

			if resp.NoChange {
				fmt.Println("No changes detected.")
				return nil
			}

			fmt.Printf("Published %s v%d (fingerprint: %s)\n", schemaID, resp.Version, resp.Fingerprint)

			if promote {
				promResp, err := cli.client.Promote(cmd.Context(), &registrypb.PromoteRequest{
					NamespaceId: ns,
				})
				if err != nil {
					return fmt.Errorf("promote failed: %w", err)
				}
				for _, p := range promResp.Promoted {
					fmt.Printf("  Promoted %s → v%d\n", p.SchemaId, p.CurrentVersion)
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&createdBy, "created-by", os.Getenv("USER"), "author of this version")
	cmd.Flags().StringArrayVar(&metadata, "metadata", nil, "metadata key=value pairs")
	cmd.Flags().BoolVar(&promote, "promote", false, "promote immediately after publish")
	cmd.Flags().BoolVar(&force, "force", false, "allow shadowing well-known types")

	return cmd
}

// resolveProtoSources reads .proto files from the given paths (files or dirs)
// and returns a map of import-path → content.
func resolveProtoSources(paths []string) (map[string][]byte, error) {
	sources := make(map[string][]byte)

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", p, err)
		}

		if !info.IsDir() {
			if !strings.HasSuffix(p, ".proto") {
				return nil, fmt.Errorf("%s is not a .proto file", p)
			}
			content, err := os.ReadFile(p) // #nosec G304 -- CLI-supplied .proto path
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", p, err)
			}
			sources[filepath.Base(p)] = content
			continue
		}

		// Walk directory.
		err = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".proto") {
				return nil
			}
			content, err := os.ReadFile(path) // #nosec G304,G122 -- CLI-supplied directory walked via filepath.WalkDir; operator-chosen path
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			rel, err := filepath.Rel(p, path)
			if err != nil {
				return err
			}
			key := filepath.ToSlash(rel)
			if _, exists := sources[key]; exists {
				return fmt.Errorf("duplicate proto file key %q from %s", key, path)
			}
			sources[key] = content
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return sources, nil
}
