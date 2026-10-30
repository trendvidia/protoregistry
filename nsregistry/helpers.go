// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// SPDX-License-Identifier: BSD-3-Clause

package nsregistry

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// The helpers below are copied from reflect/protoregistry, which does not
// export them.

type (
	typesByName         map[protoreflect.FullName]any
	extensionsByMessage map[protoreflect.FullName]extensionsByNumber
	extensionsByNumber  map[protoreflect.FieldNumber]protoreflect.ExtensionType
)

// packageDescriptor is the placeholder for a package in descsByName, and lists
// the files in that package.
type packageDescriptor struct {
	files []protoreflect.FileDescriptor
}

type nameSuffix string

func (s *nameSuffix) Pop() (name protoreflect.Name) {
	if i := strings.IndexByte(string(*s), '.'); i >= 0 {
		name, *s = protoreflect.Name((*s)[:i]), (*s)[i+1:]
	} else {
		name, *s = protoreflect.Name((*s)), ""
	}
	return name
}

// rangeTopLevelDescriptors iterates over all top-level descriptors in a file
// which will be directly entered into the registry.
func rangeTopLevelDescriptors(fd protoreflect.FileDescriptor, f func(protoreflect.Descriptor)) {
	eds := fd.Enums()
	for i := eds.Len() - 1; i >= 0; i-- {
		f(eds.Get(i))
		vds := eds.Get(i).Values()
		for i := vds.Len() - 1; i >= 0; i-- {
			f(vds.Get(i))
		}
	}
	mds := fd.Messages()
	for i := mds.Len() - 1; i >= 0; i-- {
		f(mds.Get(i))
	}
	xds := fd.Extensions()
	for i := xds.Len() - 1; i >= 0; i-- {
		f(xds.Get(i))
	}
	sds := fd.Services()
	for i := sds.Len() - 1; i >= 0; i-- {
		f(sds.Get(i))
	}
}

func findDescriptorInMessage(md protoreflect.MessageDescriptor, suffix nameSuffix) protoreflect.Descriptor {
	name := suffix.Pop()
	if suffix == "" {
		if ed := md.Enums().ByName(name); ed != nil {
			return ed
		}
		for i := md.Enums().Len() - 1; i >= 0; i-- {
			if vd := md.Enums().Get(i).Values().ByName(name); vd != nil {
				return vd
			}
		}
		if xd := md.Extensions().ByName(name); xd != nil {
			return xd
		}
		if fd := md.Fields().ByName(name); fd != nil {
			return fd
		}
		if od := md.Oneofs().ByName(name); od != nil {
			return od
		}
	}
	if md := md.Messages().ByName(name); md != nil {
		if suffix == "" {
			return md
		}
		return findDescriptorInMessage(md, suffix)
	}
	return nil
}

func typeName(t any) string {
	switch t.(type) {
	case protoreflect.EnumType:
		return "enum"
	case protoreflect.MessageType:
		return "message"
	case protoreflect.ExtensionType:
		return "extension"
	default:
		return fmt.Sprintf("%T", t)
	}
}
