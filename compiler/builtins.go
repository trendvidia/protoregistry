// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadBuiltIns reads all .proto files from dir recursively and returns them
// as DepSource entries suitable for passing to Compile as builtins.
// The filenames are relative to dir, matching proto import paths.
func LoadBuiltIns(dir string) ([]DepSource, error) {
	var sources []DepSource
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}
		content, err := os.ReadFile(path) // #nosec G304,G122 -- caller-supplied directory walked by filepath.WalkDir; symlink TOCTOU not in scope for this internal compile path
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("computing relative path for %s: %w", path, err)
		}
		sources = append(sources, DepSource{
			Filename: filepath.ToSlash(rel),
			Source:   content,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking builtins directory %s: %w", dir, err)
	}
	return sources, nil
}
