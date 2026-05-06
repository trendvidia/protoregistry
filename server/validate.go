// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"path"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// idPattern is the allowed character set for namespace and schema IDs. The
// first character must be a letter or digit; subsequent characters may also
// include `_`, `-`, and `.`. Length is enforced separately by the caller so
// the regexp does not have to encode the maximum.
//
// The reserved __builtins__ namespace contains underscores at both ends and
// is therefore *not* allowed by this pattern; it is whitelisted explicitly
// at the call site for the (privileged) bootstrap path.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// validateID checks that a user-supplied identifier (namespace, schema,
// version cursor) is non-empty, within the length bound, and uses only
// characters safe to embed in URLs, log lines, and SQL parameter values.
func validateID(value, fieldName string, maxLen int) error {
	if value == "" {
		return status.Errorf(codes.InvalidArgument, "%s is required", fieldName)
	}
	if len(value) > maxLen {
		return status.Errorf(codes.InvalidArgument, "%s exceeds %d byte limit", fieldName, maxLen)
	}
	if !idPattern.MatchString(value) {
		return status.Errorf(codes.InvalidArgument,
			"%s must match %s", fieldName, idPattern.String())
	}
	return nil
}

// validateFilename enforces that a source filename inside a Publish request
// is a clean, relative path with no traversal. Returning the rejection here
// — before the file is hashed, stored, or compiled — closes the path-traversal
// vector identified in the security audit.
func validateFilename(name string, maxLen int) error {
	if name == "" {
		return status.Error(codes.InvalidArgument, "source filename must not be empty")
	}
	if len(name) > maxLen {
		return status.Errorf(codes.InvalidArgument,
			"source filename exceeds %d byte limit", maxLen)
	}
	if strings.ContainsRune(name, 0) {
		return status.Error(codes.InvalidArgument, "source filename contains NUL byte")
	}
	if strings.HasPrefix(name, "/") {
		return status.Error(codes.InvalidArgument, "source filename must be relative")
	}
	// path.Clean canonicalises the input. Equality with the original means
	// the caller did not embed `..`, redundant `./`, or trailing slashes.
	if path.Clean(name) != name {
		return status.Errorf(codes.InvalidArgument,
			"source filename %q is not in canonical form", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return status.Errorf(codes.InvalidArgument,
				"source filename %q contains parent traversal", name)
		}
	}
	return nil
}

// validateSources runs validateFilename across every key, enforces the
// per-file and aggregate size caps, and the file-count cap. It produces
// gRPC-typed errors so the handler can return them directly.
func validateSources(sources map[string][]byte, l Limits) error {
	if len(sources) == 0 {
		return status.Error(codes.InvalidArgument, "sources must not be empty")
	}
	if len(sources) > l.MaxSourcesPerRequest {
		return status.Errorf(codes.InvalidArgument,
			"too many sources (%d > %d)", len(sources), l.MaxSourcesPerRequest)
	}
	var total int64
	for filename, src := range sources {
		if err := validateFilename(filename, l.MaxFilenameLength); err != nil {
			return err
		}
		size := int64(len(src))
		if size > l.MaxFileSourceBytes {
			return status.Errorf(codes.InvalidArgument,
				"source %q exceeds %d byte limit (%d bytes)",
				filename, l.MaxFileSourceBytes, size)
		}
		total += size
	}
	if total > l.MaxTotalSourceBytes {
		return status.Errorf(codes.InvalidArgument,
			"total source size %d exceeds %d byte limit",
			total, l.MaxTotalSourceBytes)
	}
	return nil
}
