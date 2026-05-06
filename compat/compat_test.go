// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package compat_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/trendvidia/protoregistry/compat"
	"github.com/trendvidia/protoregistry/snapshot"
)

// makeSnapshot creates a snapshot containing a single file "test.proto" with
// a single message "Config" that has the given fields.
func makeSnapshot(t *testing.T, version uint64, fields []*descriptorpb.FieldDescriptorProto) *snapshot.Snapshot {
	t.Helper()
	fds := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{
			{
				Name:    proto.String("test.proto"),
				Package: proto.String("test"),
				Syntax:  proto.String("proto3"),
				MessageType: []*descriptorpb.DescriptorProto{
					{
						Name:  proto.String("Config"),
						Field: fields,
					},
				},
			},
		},
	}
	snap, err := snapshot.New(version, fds)
	require.NoError(t, err)
	return snap
}

// makeSnapshotWithEnum creates a snapshot containing a single file with a
// message and a top-level enum.
func makeSnapshotWithEnum(t *testing.T, version uint64, enumValues []*descriptorpb.EnumValueDescriptorProto) *snapshot.Snapshot {
	t.Helper()
	fds := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{
			{
				Name:    proto.String("test.proto"),
				Package: proto.String("test"),
				Syntax:  proto.String("proto3"),
				EnumType: []*descriptorpb.EnumDescriptorProto{
					{
						Name:  proto.String("Status"),
						Value: enumValues,
					},
				},
			},
		},
	}
	snap, err := snapshot.New(version, fds)
	require.NoError(t, err)
	return snap
}

// makeSnapshotFromFDS creates a snapshot from a full FileDescriptorSet.
func makeSnapshotFromFDS(t *testing.T, version uint64, fds *descriptorpb.FileDescriptorSet) *snapshot.Snapshot {
	t.Helper()
	snap, err := snapshot.New(version, fds)
	require.NoError(t, err)
	return snap
}

func TestCheck_NilOldSnapshot(t *testing.T) {
	newSnap := makeSnapshot(t, 1, []*descriptorpb.FieldDescriptorProto{
		{
			Name:   proto.String("timeout_ms"),
			Number: proto.Int32(1),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		},
	})

	result := compat.Check(nil, newSnap)
	require.True(t, result.OK())
	assert.Empty(t, result.Violations)
}

func TestCheck_IdenticalSnapshots(t *testing.T) {
	fields := []*descriptorpb.FieldDescriptorProto{
		{
			Name:   proto.String("timeout_ms"),
			Number: proto.Int32(1),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		},
		{
			Name:   proto.String("name"),
			Number: proto.Int32(2),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		},
	}

	oldSnap := makeSnapshot(t, 1, fields)
	newSnap := makeSnapshot(t, 2, fields)

	result := compat.Check(oldSnap, newSnap)
	require.True(t, result.OK())
	assert.Empty(t, result.Violations)
}

func TestCheck_FieldRemoved(t *testing.T) {
	oldFields := []*descriptorpb.FieldDescriptorProto{
		{
			Name:   proto.String("timeout_ms"),
			Number: proto.Int32(1),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		},
		{
			Name:   proto.String("name"),
			Number: proto.Int32(2),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		},
	}
	newFields := []*descriptorpb.FieldDescriptorProto{
		{
			Name:   proto.String("timeout_ms"),
			Number: proto.Int32(1),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		},
	}

	oldSnap := makeSnapshot(t, 1, oldFields)
	newSnap := makeSnapshot(t, 2, newFields)

	result := compat.Check(oldSnap, newSnap)
	require.False(t, result.OK())
	require.Len(t, result.Violations, 1)
	assert.Equal(t, "FIELD_NO_DELETE", result.Violations[0].Rule)
	assert.Equal(t, "test.Config.name", result.Violations[0].Path)
	assert.Equal(t, "test.proto", result.Violations[0].File)
}

func TestCheck_FieldTypeChanged(t *testing.T) {
	oldFields := []*descriptorpb.FieldDescriptorProto{
		{
			Name:   proto.String("timeout_ms"),
			Number: proto.Int32(1),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		},
	}
	newFields := []*descriptorpb.FieldDescriptorProto{
		{
			Name:   proto.String("timeout_ms"),
			Number: proto.Int32(1),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		},
	}

	oldSnap := makeSnapshot(t, 1, oldFields)
	newSnap := makeSnapshot(t, 2, newFields)

	result := compat.Check(oldSnap, newSnap)
	require.False(t, result.OK())

	var found bool
	for _, v := range result.Violations {
		if v.Rule == "FIELD_SAME_TYPE" {
			found = true
			assert.Equal(t, "test.Config.timeout_ms", v.Path)
			assert.Equal(t, "test.proto", v.File)
			assert.Contains(t, v.Message, "int64")
			assert.Contains(t, v.Message, "string")
		}
	}
	assert.True(t, found, "expected FIELD_SAME_TYPE violation")
}

func TestCheck_EnumValueRemoved(t *testing.T) {
	oldValues := []*descriptorpb.EnumValueDescriptorProto{
		{Name: proto.String("STATUS_UNSPECIFIED"), Number: proto.Int32(0)},
		{Name: proto.String("STATUS_ACTIVE"), Number: proto.Int32(1)},
		{Name: proto.String("STATUS_INACTIVE"), Number: proto.Int32(2)},
	}
	newValues := []*descriptorpb.EnumValueDescriptorProto{
		{Name: proto.String("STATUS_UNSPECIFIED"), Number: proto.Int32(0)},
		{Name: proto.String("STATUS_ACTIVE"), Number: proto.Int32(1)},
	}

	oldSnap := makeSnapshotWithEnum(t, 1, oldValues)
	newSnap := makeSnapshotWithEnum(t, 2, newValues)

	result := compat.Check(oldSnap, newSnap)
	require.False(t, result.OK())
	require.Len(t, result.Violations, 1)
	assert.Equal(t, "ENUM_VALUE_NO_DELETE", result.Violations[0].Rule)
	assert.Equal(t, "test.STATUS_INACTIVE", result.Violations[0].Path)
	assert.Equal(t, "test.proto", result.Violations[0].File)
}

func TestCheck_MessageRemoved(t *testing.T) {
	oldFDS := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{
			{
				Name:    proto.String("test.proto"),
				Package: proto.String("test"),
				Syntax:  proto.String("proto3"),
				MessageType: []*descriptorpb.DescriptorProto{
					{
						Name: proto.String("Config"),
						Field: []*descriptorpb.FieldDescriptorProto{
							{
								Name:   proto.String("timeout_ms"),
								Number: proto.Int32(1),
								Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
								Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
							},
						},
					},
					{
						Name: proto.String("Settings"),
						Field: []*descriptorpb.FieldDescriptorProto{
							{
								Name:   proto.String("value"),
								Number: proto.Int32(1),
								Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
								Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
							},
						},
					},
				},
			},
		},
	}

	newFDS := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{
			{
				Name:    proto.String("test.proto"),
				Package: proto.String("test"),
				Syntax:  proto.String("proto3"),
				MessageType: []*descriptorpb.DescriptorProto{
					{
						Name: proto.String("Config"),
						Field: []*descriptorpb.FieldDescriptorProto{
							{
								Name:   proto.String("timeout_ms"),
								Number: proto.Int32(1),
								Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
								Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
							},
						},
					},
				},
			},
		},
	}

	oldSnap := makeSnapshotFromFDS(t, 1, oldFDS)
	newSnap := makeSnapshotFromFDS(t, 2, newFDS)

	result := compat.Check(oldSnap, newSnap)
	require.False(t, result.OK())
	require.Len(t, result.Violations, 1)
	assert.Equal(t, "MESSAGE_NO_DELETE", result.Violations[0].Rule)
	assert.Equal(t, "test.Settings", result.Violations[0].Path)
	assert.Equal(t, "test.proto", result.Violations[0].File)
}

func TestCheck_FieldRenamed(t *testing.T) {
	oldFields := []*descriptorpb.FieldDescriptorProto{
		{
			Name:   proto.String("timeout_ms"),
			Number: proto.Int32(1),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		},
	}
	newFields := []*descriptorpb.FieldDescriptorProto{
		{
			Name:   proto.String("timeout_millis"),
			Number: proto.Int32(1),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		},
	}

	oldSnap := makeSnapshot(t, 1, oldFields)
	newSnap := makeSnapshot(t, 2, newFields)

	result := compat.Check(oldSnap, newSnap)
	require.False(t, result.OK())

	var found bool
	for _, v := range result.Violations {
		if v.Rule == "FIELD_SAME_NAME" {
			found = true
			assert.Equal(t, "test.Config.timeout_ms", v.Path)
			assert.Contains(t, v.Message, "timeout_ms")
			assert.Contains(t, v.Message, "timeout_millis")
		}
	}
	assert.True(t, found, "expected FIELD_SAME_NAME violation")
}

func TestCheck_FieldCardinalityChanged(t *testing.T) {
	oldFields := []*descriptorpb.FieldDescriptorProto{
		{
			Name:   proto.String("tags"),
			Number: proto.Int32(1),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		},
	}
	newFields := []*descriptorpb.FieldDescriptorProto{
		{
			Name:   proto.String("tags"),
			Number: proto.Int32(1),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
		},
	}

	oldSnap := makeSnapshot(t, 1, oldFields)
	newSnap := makeSnapshot(t, 2, newFields)

	result := compat.Check(oldSnap, newSnap)
	require.False(t, result.OK())

	var found bool
	for _, v := range result.Violations {
		if v.Rule == "FIELD_SAME_CARDINALITY" {
			found = true
			assert.Equal(t, "test.Config.tags", v.Path)
			assert.Contains(t, v.Message, "optional")
			assert.Contains(t, v.Message, "repeated")
		}
	}
	assert.True(t, found, "expected FIELD_SAME_CARDINALITY violation")
}

func TestViolation_Error(t *testing.T) {
	v := &compat.Violation{
		File:    "test.proto",
		Path:    "test.Config.timeout_ms",
		Rule:    "FIELD_NO_DELETE",
		Message: "field timeout_ms was removed",
	}
	s := v.Error()
	assert.Contains(t, s, "FIELD_NO_DELETE")
	assert.Contains(t, s, "test.Config.timeout_ms")
	assert.Contains(t, s, "test.proto")
}

func TestResult_Error(t *testing.T) {
	t.Run("no violations", func(t *testing.T) {
		r := &compat.Result{}
		assert.Equal(t, "", r.Error())
	})

	t.Run("with violations", func(t *testing.T) {
		r := &compat.Result{
			Violations: []compat.Violation{
				{File: "a.proto", Path: "a.Msg", Rule: "MESSAGE_NO_DELETE", Message: "removed"},
				{File: "b.proto", Path: "b.Msg", Rule: "MESSAGE_NO_DELETE", Message: "removed"},
			},
		}
		s := r.Error()
		assert.Contains(t, s, "2 compatibility violation(s)")
		assert.Contains(t, s, "a.Msg")
		assert.Contains(t, s, "b.Msg")
	})
}
