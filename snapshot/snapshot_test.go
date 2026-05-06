// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package snapshot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// testFDS returns a simple FileDescriptorSet with a single file containing
// a message type "test.Config" with a string field "name" and a nested
// enum "Status".
func testFDS() *descriptorpb.FileDescriptorSet {
	return &descriptorpb.FileDescriptorSet{
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
								Name:     proto.String("name"),
								Number:   proto.Int32(1),
								Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
								Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
								JsonName: proto.String("name"),
							},
							{
								Name:     proto.String("value"),
								Number:   proto.Int32(2),
								Type:     descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
								Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
								JsonName: proto.String("value"),
							},
						},
						EnumType: []*descriptorpb.EnumDescriptorProto{
							{
								Name: proto.String("Status"),
								Value: []*descriptorpb.EnumValueDescriptorProto{
									{Name: proto.String("UNKNOWN"), Number: proto.Int32(0)},
									{Name: proto.String("ACTIVE"), Number: proto.Int32(1)},
								},
							},
						},
					},
				},
				EnumType: []*descriptorpb.EnumDescriptorProto{
					{
						Name: proto.String("Level"),
						Value: []*descriptorpb.EnumValueDescriptorProto{
							{Name: proto.String("LOW"), Number: proto.Int32(0)},
							{Name: proto.String("HIGH"), Number: proto.Int32(1)},
						},
					},
				},
			},
		},
	}
}

// testMultiFileFDS returns a FileDescriptorSet with two files where the second
// file depends on the first.
func testMultiFileFDS() *descriptorpb.FileDescriptorSet {
	return &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{
			{
				Name:    proto.String("base.proto"),
				Package: proto.String("base"),
				Syntax:  proto.String("proto3"),
				MessageType: []*descriptorpb.DescriptorProto{
					{
						Name: proto.String("Timestamp"),
						Field: []*descriptorpb.FieldDescriptorProto{
							{
								Name:     proto.String("seconds"),
								Number:   proto.Int32(1),
								Type:     descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
								Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
								JsonName: proto.String("seconds"),
							},
						},
					},
				},
			},
			{
				Name:       proto.String("event.proto"),
				Package:    proto.String("event"),
				Syntax:     proto.String("proto3"),
				Dependency: []string{"base.proto"},
				MessageType: []*descriptorpb.DescriptorProto{
					{
						Name: proto.String("Event"),
						Field: []*descriptorpb.FieldDescriptorProto{
							{
								Name:     proto.String("id"),
								Number:   proto.Int32(1),
								Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
								Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
								JsonName: proto.String("id"),
							},
							{
								Name:     proto.String("created_at"),
								Number:   proto.Int32(2),
								Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
								Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
								TypeName: proto.String(".base.Timestamp"),
								JsonName: proto.String("createdAt"),
							},
						},
					},
				},
			},
		},
	}
}

func TestNew(t *testing.T) {
	t.Run("valid FDS", func(t *testing.T) {
		snap, err := New(1, testFDS())
		require.NoError(t, err)
		require.NotNil(t, snap)

		assert.Equal(t, uint64(1), snap.Version())
		assert.NotNil(t, snap.Files())
		assert.NotNil(t, snap.Types())
		assert.Len(t, snap.FileDescriptors(), 1)
		assert.Equal(t, "test.proto", string(snap.FileDescriptors()[0].Path()))
	})

	t.Run("multi-file FDS", func(t *testing.T) {
		snap, err := New(42, testMultiFileFDS())
		require.NoError(t, err)
		require.NotNil(t, snap)

		assert.Equal(t, uint64(42), snap.Version())
		assert.Len(t, snap.FileDescriptors(), 2)

		// Both files should be findable.
		fd, err := snap.Files().FindFileByPath("base.proto")
		require.NoError(t, err)
		assert.Equal(t, protoreflect.FullName("base"), fd.Package())

		fd, err = snap.Files().FindFileByPath("event.proto")
		require.NoError(t, err)
		assert.Equal(t, protoreflect.FullName("event"), fd.Package())
	})

	t.Run("empty FDS", func(t *testing.T) {
		snap, err := New(0, &descriptorpb.FileDescriptorSet{})
		require.NoError(t, err)
		require.NotNil(t, snap)

		assert.Equal(t, uint64(0), snap.Version())
		assert.Empty(t, snap.FileDescriptors())
	})

	t.Run("invalid FDS returns error", func(t *testing.T) {
		// A file that references a non-existent dependency should fail.
		fds := &descriptorpb.FileDescriptorSet{
			File: []*descriptorpb.FileDescriptorProto{
				{
					Name:       proto.String("bad.proto"),
					Dependency: []string{"nonexistent.proto"},
				},
			},
		}
		_, err := New(0, fds)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "building file registry")
	})
}

func TestFindMessageByName(t *testing.T) {
	snap, err := New(1, testFDS())
	require.NoError(t, err)

	t.Run("existing message", func(t *testing.T) {
		md, err := snap.FindMessageByName("test.Config")
		require.NoError(t, err)
		assert.Equal(t, protoreflect.FullName("test.Config"), md.FullName())
		assert.Equal(t, 2, md.Fields().Len(), "expected 2 fields on test.Config")
	})

	t.Run("field details", func(t *testing.T) {
		md, err := snap.FindMessageByName("test.Config")
		require.NoError(t, err)

		nameField := md.Fields().ByName("name")
		require.NotNil(t, nameField)
		assert.Equal(t, protoreflect.StringKind, nameField.Kind())
		assert.Equal(t, protoreflect.FieldNumber(1), nameField.Number())

		valueField := md.Fields().ByName("value")
		require.NotNil(t, valueField)
		assert.Equal(t, protoreflect.Int32Kind, valueField.Kind())
		assert.Equal(t, protoreflect.FieldNumber(2), valueField.Number())
	})

	t.Run("nested enum is registered", func(t *testing.T) {
		// The nested enum test.Config.Status should be in the type registry.
		et, err := snap.Types().FindEnumByName("test.Config.Status")
		require.NoError(t, err)
		assert.Equal(t, protoreflect.FullName("test.Config.Status"), et.Descriptor().FullName())
	})

	t.Run("top-level enum is registered", func(t *testing.T) {
		et, err := snap.Types().FindEnumByName("test.Level")
		require.NoError(t, err)
		assert.Equal(t, protoreflect.FullName("test.Level"), et.Descriptor().FullName())
	})

	t.Run("nonexistent message", func(t *testing.T) {
		_, err := snap.FindMessageByName("test.DoesNotExist")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("multi-file message lookup", func(t *testing.T) {
		snap, err := New(1, testMultiFileFDS())
		require.NoError(t, err)

		md, err := snap.FindMessageByName("base.Timestamp")
		require.NoError(t, err)
		assert.Equal(t, protoreflect.FullName("base.Timestamp"), md.FullName())

		md, err = snap.FindMessageByName("event.Event")
		require.NoError(t, err)
		assert.Equal(t, protoreflect.FullName("event.Event"), md.FullName())
		assert.Equal(t, 2, md.Fields().Len())
	})
}

func TestNewMessage(t *testing.T) {
	snap, err := New(1, testFDS())
	require.NoError(t, err)

	t.Run("create and set fields", func(t *testing.T) {
		msg, err := snap.NewMessage("test.Config")
		require.NoError(t, err)
		require.NotNil(t, msg)

		// The dynamic message should be settable.
		nameField := msg.Descriptor().Fields().ByName("name")
		require.NotNil(t, nameField)
		msg.Set(nameField, protoreflect.ValueOfString("my-config"))

		got := msg.Get(nameField).String()
		assert.Equal(t, "my-config", got)

		valueField := msg.Descriptor().Fields().ByName("value")
		require.NotNil(t, valueField)
		msg.Set(valueField, protoreflect.ValueOfInt32(99))

		gotInt := msg.Get(valueField).Int()
		assert.Equal(t, int64(99), gotInt)
	})

	t.Run("message full name matches", func(t *testing.T) {
		msg, err := snap.NewMessage("test.Config")
		require.NoError(t, err)
		assert.Equal(t, protoreflect.FullName("test.Config"), msg.Descriptor().FullName())
	})

	t.Run("nonexistent message", func(t *testing.T) {
		_, err := snap.NewMessage("test.DoesNotExist")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestNewFromFiles(t *testing.T) {
	// First build file descriptors from an FDS using protodesc, then pass
	// them to NewFromFiles.
	fds := testFDS()
	files, err := protodesc.NewFiles(fds)
	require.NoError(t, err)

	var fileDescs []protoreflect.FileDescriptor
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		fileDescs = append(fileDescs, fd)
		return true
	})

	t.Run("valid file descriptors", func(t *testing.T) {
		snap, err := NewFromFiles(7, fileDescs)
		require.NoError(t, err)
		require.NotNil(t, snap)

		assert.Equal(t, uint64(7), snap.Version())
		assert.Len(t, snap.FileDescriptors(), 1)

		// Message lookup should work.
		md, err := snap.FindMessageByName("test.Config")
		require.NoError(t, err)
		assert.Equal(t, protoreflect.FullName("test.Config"), md.FullName())

		// Dynamic message creation should work.
		msg, err := snap.NewMessage("test.Config")
		require.NoError(t, err)
		require.NotNil(t, msg)
	})

	t.Run("empty file list", func(t *testing.T) {
		snap, err := NewFromFiles(0, nil)
		require.NoError(t, err)
		require.NotNil(t, snap)

		assert.Equal(t, uint64(0), snap.Version())
		assert.Empty(t, snap.FileDescriptors())
	})

	t.Run("multi-file with dependencies", func(t *testing.T) {
		multiFDS := testMultiFileFDS()
		multiFiles, err := protodesc.NewFiles(multiFDS)
		require.NoError(t, err)

		var multiDescs []protoreflect.FileDescriptor
		multiFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
			multiDescs = append(multiDescs, fd)
			return true
		})

		snap, err := NewFromFiles(10, multiDescs)
		require.NoError(t, err)
		assert.Len(t, snap.FileDescriptors(), 2)

		md, err := snap.FindMessageByName("event.Event")
		require.NoError(t, err)
		assert.Equal(t, protoreflect.FullName("event.Event"), md.FullName())
	})

	t.Run("duplicate file returns error", func(t *testing.T) {
		// Passing the same file descriptor twice should fail on the second
		// registration.
		doubled := append(fileDescs, fileDescs...)
		_, err := NewFromFiles(0, doubled)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "registering file")
	})
}

func TestVersion(t *testing.T) {
	tests := []uint64{0, 1, 42, 1<<64 - 1}
	for _, v := range tests {
		snap, err := New(v, &descriptorpb.FileDescriptorSet{})
		require.NoError(t, err)
		assert.Equal(t, v, snap.Version())
	}
}
