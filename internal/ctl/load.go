// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package ctl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
	"github.com/spf13/cobra"

	registrypb "github.com/trendvidia/protoregistry/proto/protoregistry/v1"
)

func newLoadCmd(cli *CLI, defaultNS *string) *cobra.Command {
	var (
		createdBy string
		promote   bool
		force     bool
	)

	cmd := &cobra.Command{
		Use:   "load [namespace] <path>",
		Short: "Load all proto files from a directory in dependency order",
		Long: `Walks a directory for .proto files, resolves their import dependencies,
and publishes each file as a separate schema in topological order.

Each proto file becomes its own schema (schema ID = relative path).
Files are published in dependency order so that imports resolve against
previously published schemas in the same namespace.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var ns, dir string
			if len(args) == 2 {
				ns, dir = args[0], args[1]
			} else {
				var err error
				ns, err = resolveNamespace(nil, 0, defaultNS)
				if err != nil {
					return fmt.Errorf("namespace is required as first argument or via --namespace")
				}
				dir = args[0]
			}

			return runLoad(cmd, cli, ns, dir, createdBy, promote, force)
		},
	}

	cmd.Flags().StringVar(&createdBy, "created-by", os.Getenv("USER"), "author of this version")
	cmd.Flags().BoolVar(&promote, "promote", false, "promote all schemas after loading")
	cmd.Flags().BoolVar(&force, "force", false, "allow shadowing well-known types")

	return cmd
}

// protoFile holds a parsed proto file and its import list.
type protoFile struct {
	filename string
	content  []byte
	imports  []string
}

func runLoad(cmd *cobra.Command, cli *CLI, ns, dir, createdBy string, promote, force bool) error {
	// 1. Walk directory and parse all proto files for imports.
	protos, err := scanProtos(dir)
	if err != nil {
		return err
	}
	if len(protos) == 0 {
		return fmt.Errorf("no .proto files found in %s", dir)
	}

	fmt.Printf("Found %d proto file(s) in %s\n", len(protos), dir)

	// Build a set of files we're loading for quick lookup.
	fileSet := make(map[string]bool, len(protos))
	for _, p := range protos {
		fileSet[p.filename] = true
	}

	// 2. Topological sort via iterative loading (same approach as LoadSpecs).
	loaded := make(map[string]bool)
	failed := make(map[string]error)
	ctx := cmd.Context()

	for {
		progress := false

		for _, p := range protos {
			if loaded[p.filename] || failed[p.filename] != nil {
				continue
			}

			canPublish := true
			depFailed := false

			for _, imp := range p.imports {
				if loaded[imp] {
					continue // Already published in this session.
				}

				if fileSet[imp] {
					// Dependency is in our set but not yet loaded — wait.
					if failed[imp] != nil {
						failed[p.filename] = fmt.Errorf("dependency %s failed: %w", imp, failed[imp])
						depFailed = true
						break
					}
					canPublish = false
					break
				}

				// Not in our set — it's either a well-known type, a builtin,
				// or already exists in the namespace. The compiler will resolve
				// it; we don't need to check here.
			}

			if depFailed {
				continue
			}

			if !canPublish {
				continue
			}

			// Publish this file as its own schema.
			resp, err := cli.client.Publish(ctx, &registrypb.PublishRequest{
				NamespaceId: ns,
				SchemaId:    p.filename,
				Sources:     map[string][]byte{p.filename: p.content},
				CreatedBy:   createdBy,
				Force:       force,
			})
			if err != nil {
				failed[p.filename] = err
				continue
			}

			loaded[p.filename] = true
			progress = true

			if resp.NoChange {
				fmt.Printf("  %-50s (no change)\n", p.filename)
			} else {
				fmt.Printf("  %-50s v%d\n", p.filename, resp.Version)
			}
		}

		if !progress {
			break
		}
	}

	// Report failures.
	if len(failed) > 0 {
		fmt.Printf("\nFailed to load %d file(s):\n", len(failed))
		for name, reason := range failed {
			fmt.Printf("  %s: %v\n", name, reason)
		}
	}

	// Promote if requested.
	if promote && len(loaded) > 0 {
		resp, err := cli.client.Promote(ctx, &registrypb.PromoteRequest{
			NamespaceId: ns,
		})
		if err != nil {
			return fmt.Errorf("promote failed: %w", err)
		}
		fmt.Printf("\nPromoted %d schema(s):\n", len(resp.Promoted))
		for _, p := range resp.Promoted {
			fmt.Printf("  %s → v%d\n", p.SchemaId, p.CurrentVersion)
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("%d file(s) failed to load", len(failed))
	}

	fmt.Printf("\nLoaded %d proto file(s) into namespace %q.\n", len(loaded), ns)
	return nil
}

// scanProtos walks dir recursively, parses each .proto file to extract
// its import list, and returns the results.
func scanProtos(dir string) ([]protoFile, error) {
	dir = filepath.Clean(dir)
	var protos []protoFile

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}

		content, err := os.ReadFile(path) // #nosec G304,G122 -- CLI-supplied path walked via filepath.WalkDir; operator-chosen directory
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		filename := filepath.ToSlash(rel)

		imports, err := extractImports(filename, content)
		if err != nil {
			return fmt.Errorf("parsing imports from %s: %w", filename, err)
		}

		protos = append(protos, protoFile{
			filename: filename,
			content:  content,
			imports:  imports,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", dir, err)
	}
	return protos, nil
}

// extractImports parses a proto file and returns its import paths.
func extractImports(filename string, content []byte) ([]string, error) {
	node, err := parser.Parse(filename, strings.NewReader(string(content)), reporter.NewHandler(nil))
	if err != nil {
		return nil, err
	}
	result, err := parser.ResultFromAST(node, true, reporter.NewHandler(nil))
	if err != nil {
		return nil, err
	}
	deps := result.FileDescriptorProto().Dependency
	imports := make([]string, len(deps))
	for i, dep := range deps {
		imports[i] = strings.TrimSpace(dep)
	}
	return imports, nil
}
