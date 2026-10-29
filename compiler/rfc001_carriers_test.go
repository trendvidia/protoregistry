// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package compiler

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/trendvidia/protocompile"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/trendvidia/protoregistry/snapshot"
)

// The RFC-001 carriers (Protowire Schema Extensions, draft -01): the
// option extensions protowire.schema.v1 declares on FileOptions and the
// per-kind *Options messages. file_annotations and the per-kind
// annotation lists share 1327.
const (
	carrierAnnotations    = 1327
	carrierFunctions      = 1328
	carrierAnnotationDecl = 1329
	carrierTypeDecls      = 1330
	carrierSourceMap      = 1331
)

// rfc001Builtins are the files an RFC-001 schema imports that protoregistry
// does not bundle: protowire's annotations.proto, and the legacy
// pxf/annotations.proto one fixture uses. A namespace supplies them through
// __builtins__.
func rfc001Builtins(t *testing.T) []DepSource {
	t.Helper()
	var out []DepSource
	for _, name := range []string{"protowire/schema/v1/annotations.proto", "pxf/annotations.proto"} {
		b, err := os.ReadFile(filepath.Join("testdata/protowire-builtins", name))
		require.NoError(t, err)
		out = append(out, DepSource{Namespace: "__builtins__", SchemaID: "protowire", Version: 1, Filename: name, Source: b})
	}
	return out
}

// rfc001Fixtures reads protowire's testdata/schema-extensions, grouped into
// the files that compile together: one group per file, except the 06_
// cross-file pair and the 07_report_golden tree.
func rfc001Fixtures(t *testing.T) map[string]map[string][]byte {
	t.Helper()
	const root = "testdata/schema-extensions"
	groups := map[string]map[string][]byte{}
	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".proto") {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		group := rel
		switch {
		case strings.HasPrefix(rel, "07_report_golden/"):
			group, rel = "07_report_golden", strings.TrimPrefix(rel, "07_report_golden/")
		case strings.HasPrefix(rel, "06_"):
			group = "06_cross_file"
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if groups[group] == nil {
			groups[group] = map[string][]byte{}
		}
		groups[group][rel] = src
		return nil
	}))
	require.NotEmpty(t, groups)
	return groups
}

// carriers returns the RFC-001 carrier field numbers set on every *Options
// message in fdp, one entry per occurrence.
func carriers(t *testing.T, fdp *descriptorpb.FileDescriptorProto) []protowire.Number {
	t.Helper()
	var out []protowire.Number
	var walk func(m protoreflect.Message)
	walk = func(m protoreflect.Message) {
		if strings.HasSuffix(string(m.Descriptor().Name()), "Options") {
			b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m.Interface())
			require.NoError(t, err)
			for len(b) > 0 {
				num, _, n := protowire.ConsumeField(b)
				require.GreaterOrEqual(t, n, 0, "malformed options in %s", fdp.GetName())
				if num >= carrierAnnotations && num <= carrierSourceMap {
					out = append(out, num)
				}
				b = b[n:]
			}
			return
		}
		m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			switch {
			case fd.Message() == nil:
			case fd.IsList():
				for i := range v.List().Len() {
					walk(v.List().Get(i).Message())
				}
			case !fd.IsMap():
				walk(v.Message())
			}
			return true
		})
	}
	walk(fdp.ProtoReflect())
	return out
}

func marshalFile(t *testing.T, fdp *descriptorpb.FileDescriptorProto) []byte {
	t.Helper()
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(fdp)
	require.NoError(t, err)
	return b
}

// compileDirect compiles a group with the protocompile fork alone, outside
// the registry, as protowire's toolchain does.
func compileDirect(t *testing.T, sources map[string][]byte, builtins []DepSource) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for name, src := range sources {
		files[name] = src
	}
	for _, b := range builtins {
		files[b.Filename] = b.Source
	}
	resolver := protocompile.WithStandardImports(protocompile.ResolverFunc(func(path string) (protocompile.SearchResult, error) {
		if src, ok := files[path]; ok {
			return protocompile.SearchResult{Source: bytes.NewReader(src)}, nil
		}
		return protocompile.SearchResult{}, fs.ErrNotExist
	}))
	var names []string
	for name := range sources {
		names = append(names, name)
	}
	compiled, err := (&protocompile.Compiler{Resolver: resolver}).Compile(context.Background(), names...)
	require.NoError(t, err)
	out := map[string][]byte{}
	for _, fd := range compiled {
		out[fd.Path()] = marshalFile(t, protodesc.ToFileDescriptorProto(fd))
	}
	return out
}

// A schema using RFC-001 syntax publishes, and the carriers the compiler
// emits survive storage unchanged: the stored descriptors equal what the
// toolchain alone produces, byte for byte, and they come back byte-identical
// through both read paths — snapshot.New (Restore) and the GetDescriptor
// decode/re-encode. Fixtures: protowire testdata/schema-extensions.
func TestCompile_RFC001CarriersRoundTrip(t *testing.T) {
	builtins := rfc001Builtins(t)
	groups := rfc001Fixtures(t)
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)

	seen := map[protowire.Number]bool{}
	for _, name := range names {
		sources := groups[name]
		t.Run(name, func(t *testing.T) {
			res, err := New().Compile(context.Background(), 1, sources, nil, nil, builtins)
			require.NoError(t, err)

			stored := &descriptorpb.FileDescriptorSet{}
			require.NoError(t, proto.Unmarshal(res.Compiled, stored))
			direct := compileDirect(t, sources, builtins)

			restored, err := snapshot.New(1, stored)
			require.NoError(t, err)

			// GetDescriptor unmarshals the stored bytes and the RPC layer
			// marshals them again.
			served := &descriptorpb.FileDescriptorSet{}
			reencoded, err := proto.Marshal(stored)
			require.NoError(t, err)
			require.NoError(t, proto.Unmarshal(reencoded, served))
			servedByName := map[string]*descriptorpb.FileDescriptorProto{}
			for _, f := range served.GetFile() {
				servedByName[f.GetName()] = f
			}

			own := 0
			for _, f := range stored.GetFile() {
				if _, ok := sources[f.GetName()]; !ok {
					continue
				}
				own++
				got := carriers(t, f)
				assert.NotEmpty(t, got, "%s has no RFC-001 carrier", f.GetName())
				for _, n := range got {
					seen[n] = true
				}

				want := marshalFile(t, f)
				assert.Equal(t, direct[f.GetName()], want, "%s: stored descriptor differs from the toolchain's", f.GetName())

				fd, err := restored.Files().FindFileByPath(f.GetName())
				require.NoError(t, err)
				assert.Equal(t, want, marshalFile(t, protodesc.ToFileDescriptorProto(fd)), "%s: changed by snapshot.New", f.GetName())

				require.Contains(t, servedByName, f.GetName())
				assert.Equal(t, want, marshalFile(t, servedByName[f.GetName()]), "%s: changed by the GetDescriptor round trip", f.GetName())
			}
			assert.Equal(t, len(sources), own, "every fixture file is in the stored set")
		})
	}

	for _, n := range []protowire.Number{carrierAnnotations, carrierFunctions, carrierAnnotationDecl, carrierTypeDecls, carrierSourceMap} {
		assert.True(t, seen[n], "no fixture exercised carrier field %d", n)
	}
}
