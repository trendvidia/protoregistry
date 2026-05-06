// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package compat checks backward compatibility between schema versions.
// It walks both descriptor trees and reports violations that would break
// existing consumers relying on protobuf wire compatibility.
package compat

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protoregistry/snapshot"
)

// Violation describes a single backward compatibility violation.
type Violation struct {
	// File is the proto file where the violation occurred.
	File string
	// Path is the fully-qualified path of the element (e.g. "acme.Config.timeout_ms").
	Path string
	// Rule is the machine-readable rule identifier.
	Rule string
	// Message is a human-readable description.
	Message string
}

// Error implements the error interface for a single violation.
func (v *Violation) Error() string {
	return fmt.Sprintf("%s: %s at %s (%s)", v.Rule, v.Message, v.Path, v.File)
}

// Result holds the outcome of a compatibility check.
type Result struct {
	Violations []Violation
}

// OK returns true if there are no violations.
func (r *Result) OK() bool {
	return len(r.Violations) == 0
}

// Error returns all violations as a single error string.
func (r *Result) Error() string {
	if r.OK() {
		return ""
	}
	s := fmt.Sprintf("%d compatibility violation(s):", len(r.Violations))
	for _, v := range r.Violations {
		s += "\n  - " + v.Error()
	}
	return s
}

// Check compares old and new snapshots for backward compatibility violations.
// It checks all files in the old snapshot against corresponding files in
// the new snapshot.
func Check(old, new *snapshot.Snapshot) *Result {
	result := &Result{}
	if old == nil {
		return result
	}

	old.Files().RangeFiles(func(oldFile protoreflect.FileDescriptor) bool {
		newFile, err := new.Files().FindFileByPath(string(oldFile.Path()))
		if err != nil {
			result.Violations = append(result.Violations, Violation{
				File:    string(oldFile.Path()),
				Path:    string(oldFile.Path()),
				Rule:    "FILE_NO_DELETE",
				Message: "file was removed",
			})
			return true
		}
		checkFile(result, oldFile, newFile)
		return true
	})

	return result
}

func checkFile(result *Result, oldFile, newFile protoreflect.FileDescriptor) {
	// Check messages.
	for i := range oldFile.Messages().Len() {
		oldMsg := oldFile.Messages().Get(i)
		checkMessage(result, oldFile, oldMsg, newFile.Messages())
	}
	// Check enums.
	for i := range oldFile.Enums().Len() {
		oldEnum := oldFile.Enums().Get(i)
		checkEnum(result, oldFile, oldEnum, newFile.Enums())
	}
	// Check services.
	for i := range oldFile.Services().Len() {
		oldSvc := oldFile.Services().Get(i)
		checkService(result, oldFile, oldSvc, newFile.Services())
	}
}

func checkMessage(result *Result, file protoreflect.FileDescriptor, oldMsg protoreflect.MessageDescriptor, newMsgs protoreflect.MessageDescriptors) {
	newMsg := newMsgs.ByName(oldMsg.Name())
	if newMsg == nil {
		result.Violations = append(result.Violations, Violation{
			File:    string(file.Path()),
			Path:    string(oldMsg.FullName()),
			Rule:    "MESSAGE_NO_DELETE",
			Message: fmt.Sprintf("message %s was removed", oldMsg.Name()),
		})
		return
	}

	// Check fields.
	for i := range oldMsg.Fields().Len() {
		oldField := oldMsg.Fields().Get(i)
		checkField(result, file, oldMsg, oldField, newMsg.Fields())
	}

	// Check nested messages.
	for i := range oldMsg.Messages().Len() {
		checkMessage(result, file, oldMsg.Messages().Get(i), newMsg.Messages())
	}

	// Check nested enums.
	for i := range oldMsg.Enums().Len() {
		checkEnum(result, file, oldMsg.Enums().Get(i), newMsg.Enums())
	}

	// Check oneofs.
	for i := range oldMsg.Oneofs().Len() {
		oldOneof := oldMsg.Oneofs().Get(i)
		newOneof := newMsg.Oneofs().ByName(oldOneof.Name())
		if newOneof == nil {
			result.Violations = append(result.Violations, Violation{
				File:    string(file.Path()),
				Path:    string(oldOneof.FullName()),
				Rule:    "ONEOF_NO_DELETE",
				Message: fmt.Sprintf("oneof %s was removed", oldOneof.Name()),
			})
		}
	}
}

func checkField(result *Result, file protoreflect.FileDescriptor, msg protoreflect.MessageDescriptor, oldField protoreflect.FieldDescriptor, newFields protoreflect.FieldDescriptors) {
	newField := newFields.ByNumber(oldField.Number())
	if newField == nil {
		result.Violations = append(result.Violations, Violation{
			File:    string(file.Path()),
			Path:    string(oldField.FullName()),
			Rule:    "FIELD_NO_DELETE",
			Message: fmt.Sprintf("field %s (number %d) was removed from %s", oldField.Name(), oldField.Number(), msg.FullName()),
		})
		return
	}

	// Check type change.
	if oldField.Kind() != newField.Kind() {
		result.Violations = append(result.Violations, Violation{
			File:    string(file.Path()),
			Path:    string(oldField.FullName()),
			Rule:    "FIELD_SAME_TYPE",
			Message: fmt.Sprintf("field %s changed type from %s to %s", oldField.Name(), oldField.Kind(), newField.Kind()),
		})
	}

	// Check name change (wire-compatible but potentially breaking for JSON).
	if oldField.Name() != newField.Name() {
		result.Violations = append(result.Violations, Violation{
			File:    string(file.Path()),
			Path:    string(oldField.FullName()),
			Rule:    "FIELD_SAME_NAME",
			Message: fmt.Sprintf("field number %d renamed from %s to %s", oldField.Number(), oldField.Name(), newField.Name()),
		})
	}

	// Check label change (optional → repeated, etc.).
	if oldField.Cardinality() != newField.Cardinality() {
		result.Violations = append(result.Violations, Violation{
			File:    string(file.Path()),
			Path:    string(oldField.FullName()),
			Rule:    "FIELD_SAME_CARDINALITY",
			Message: fmt.Sprintf("field %s changed cardinality from %s to %s", oldField.Name(), oldField.Cardinality(), newField.Cardinality()),
		})
	}

	// Check message type reference change.
	if oldField.Kind() == protoreflect.MessageKind && newField.Kind() == protoreflect.MessageKind {
		if oldField.Message().FullName() != newField.Message().FullName() {
			result.Violations = append(result.Violations, Violation{
				File:    string(file.Path()),
				Path:    string(oldField.FullName()),
				Rule:    "FIELD_SAME_MESSAGE_TYPE",
				Message: fmt.Sprintf("field %s changed message type from %s to %s", oldField.Name(), oldField.Message().FullName(), newField.Message().FullName()),
			})
		}
	}

	// Check enum type reference change.
	if oldField.Kind() == protoreflect.EnumKind && newField.Kind() == protoreflect.EnumKind {
		if oldField.Enum().FullName() != newField.Enum().FullName() {
			result.Violations = append(result.Violations, Violation{
				File:    string(file.Path()),
				Path:    string(oldField.FullName()),
				Rule:    "FIELD_SAME_ENUM_TYPE",
				Message: fmt.Sprintf("field %s changed enum type from %s to %s", oldField.Name(), oldField.Enum().FullName(), newField.Enum().FullName()),
			})
		}
	}
}

func checkEnum(result *Result, file protoreflect.FileDescriptor, oldEnum protoreflect.EnumDescriptor, newEnums protoreflect.EnumDescriptors) {
	newEnum := newEnums.ByName(oldEnum.Name())
	if newEnum == nil {
		result.Violations = append(result.Violations, Violation{
			File:    string(file.Path()),
			Path:    string(oldEnum.FullName()),
			Rule:    "ENUM_NO_DELETE",
			Message: fmt.Sprintf("enum %s was removed", oldEnum.Name()),
		})
		return
	}

	// Check enum values.
	for i := range oldEnum.Values().Len() {
		oldVal := oldEnum.Values().Get(i)
		newVal := newEnum.Values().ByNumber(oldVal.Number())
		if newVal == nil {
			result.Violations = append(result.Violations, Violation{
				File:    string(file.Path()),
				Path:    string(oldVal.FullName()),
				Rule:    "ENUM_VALUE_NO_DELETE",
				Message: fmt.Sprintf("enum value %s (number %d) was removed from %s", oldVal.Name(), oldVal.Number(), oldEnum.FullName()),
			})
		}
	}
}

func checkService(result *Result, file protoreflect.FileDescriptor, oldSvc protoreflect.ServiceDescriptor, newSvcs protoreflect.ServiceDescriptors) {
	newSvc := newSvcs.ByName(oldSvc.Name())
	if newSvc == nil {
		result.Violations = append(result.Violations, Violation{
			File:    string(file.Path()),
			Path:    string(oldSvc.FullName()),
			Rule:    "SERVICE_NO_DELETE",
			Message: fmt.Sprintf("service %s was removed", oldSvc.Name()),
		})
		return
	}

	for i := range oldSvc.Methods().Len() {
		oldMethod := oldSvc.Methods().Get(i)
		newMethod := newSvc.Methods().ByName(oldMethod.Name())
		if newMethod == nil {
			result.Violations = append(result.Violations, Violation{
				File:    string(file.Path()),
				Path:    string(oldMethod.FullName()),
				Rule:    "METHOD_NO_DELETE",
				Message: fmt.Sprintf("method %s was removed from %s", oldMethod.Name(), oldSvc.FullName()),
			})
			continue
		}
		if oldMethod.Input().FullName() != newMethod.Input().FullName() {
			result.Violations = append(result.Violations, Violation{
				File:    string(file.Path()),
				Path:    string(oldMethod.FullName()),
				Rule:    "METHOD_SAME_INPUT_TYPE",
				Message: fmt.Sprintf("method %s changed input type from %s to %s", oldMethod.Name(), oldMethod.Input().FullName(), newMethod.Input().FullName()),
			})
		}
		if oldMethod.Output().FullName() != newMethod.Output().FullName() {
			result.Violations = append(result.Violations, Violation{
				File:    string(file.Path()),
				Path:    string(oldMethod.FullName()),
				Rule:    "METHOD_SAME_OUTPUT_TYPE",
				Message: fmt.Sprintf("method %s changed output type from %s to %s", oldMethod.Name(), oldMethod.Output().FullName(), newMethod.Output().FullName()),
			})
		}
	}
}
