// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package ctl

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	registrypb "github.com/trendvidia/protoregistry/proto/protoregistry/v1"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

func newPXFCmd(cli *CLI, defaultNS *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pxf",
		Short: "PXF encoding/decoding commands",
	}
	cmd.AddCommand(
		newPXFEncodeCmd(cli, defaultNS),
		newPXFDecodeCmd(cli, defaultNS),
		newPXFValidateCmd(cli, defaultNS),
		newPXFFmtCmd(cli, defaultNS),
	)
	return cmd
}

// resolveRegistryDescriptor fetches a FileDescriptorSet from the registry
// and finds the requested message type.
func resolveRegistryDescriptor(ctx context.Context, client registrypb.RegistryServiceClient, ns, schema, msgName string) (protoreflect.MessageDescriptor, error) {
	resp, err := client.GetDescriptor(ctx, &registrypb.GetDescriptorRequest{
		NamespaceId: ns,
		SchemaId:    schema,
	})
	if err != nil {
		return nil, fmt.Errorf("GetDescriptor: %w", err)
	}

	files, err := protodesc.NewFiles(resp.FileDescriptorSet)
	if err != nil {
		return nil, fmt.Errorf("build descriptors: %w", err)
	}

	fullName := protoreflect.FullName(msgName)
	desc, err := files.FindDescriptorByName(fullName)
	if err != nil {
		return nil, fmt.Errorf("message %q not found in descriptor", msgName)
	}
	md, ok := desc.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("%q is not a message type", msgName)
	}
	return md, nil
}

func newPXFEncodeCmd(cli *CLI, defaultNS *string) *cobra.Command {
	var (
		schema  string
		msgName string
	)
	cmd := &cobra.Command{
		Use:   "encode [namespace] <file.pxf>",
		Short: "Encode PXF to protobuf binary (stdout)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, file, err := nsAndFile(args, defaultNS)
			if err != nil {
				return err
			}
			desc, err := resolveRegistryDescriptor(cmd.Context(), cli.client, ns, schema, msgName)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(file) // #nosec G304 -- CLI-supplied input file path
			if err != nil {
				return err
			}
			msg, err := pxf.UnmarshalDescriptor(data, desc)
			if err != nil {
				return err
			}
			out, err := proto.Marshal(msg)
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(out)
			return err
		},
	}
	cmd.Flags().StringVar(&schema, "schema", "", "schema name (required)")
	cmd.Flags().StringVarP(&msgName, "message", "m", "", "fully qualified message name (required)")
	_ = cmd.MarkFlagRequired("schema")
	_ = cmd.MarkFlagRequired("message")
	return cmd
}

func newPXFDecodeCmd(cli *CLI, defaultNS *string) *cobra.Command {
	var (
		schema  string
		msgName string
	)
	cmd := &cobra.Command{
		Use:   "decode [namespace] <file.pb>",
		Short: "Decode protobuf binary to PXF (stdout)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, file, err := nsAndFile(args, defaultNS)
			if err != nil {
				return err
			}
			desc, err := resolveRegistryDescriptor(cmd.Context(), cli.client, ns, schema, msgName)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(file) // #nosec G304 -- CLI-supplied input file path
			if err != nil {
				return err
			}
			msg := dynamicpb.NewMessage(desc)
			if err := proto.Unmarshal(data, msg); err != nil {
				return err
			}
			out, err := pxf.Marshal(msg)
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(out)
			return err
		},
	}
	cmd.Flags().StringVar(&schema, "schema", "", "schema name (required)")
	cmd.Flags().StringVarP(&msgName, "message", "m", "", "fully qualified message name (required)")
	_ = cmd.MarkFlagRequired("schema")
	_ = cmd.MarkFlagRequired("message")
	return cmd
}

func newPXFValidateCmd(cli *CLI, defaultNS *string) *cobra.Command {
	var (
		schema  string
		msgName string
	)
	cmd := &cobra.Command{
		Use:   "validate [namespace] <file.pxf>",
		Short: "Validate PXF against registry schema",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, file, err := nsAndFile(args, defaultNS)
			if err != nil {
				return err
			}
			desc, err := resolveRegistryDescriptor(cmd.Context(), cli.client, ns, schema, msgName)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(file) // #nosec G304 -- CLI-supplied input file path
			if err != nil {
				return err
			}
			if _, err := pxf.UnmarshalDescriptor(data, desc); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "valid")
			return nil
		},
	}
	cmd.Flags().StringVar(&schema, "schema", "", "schema name (required)")
	cmd.Flags().StringVarP(&msgName, "message", "m", "", "fully qualified message name (required)")
	_ = cmd.MarkFlagRequired("schema")
	_ = cmd.MarkFlagRequired("message")
	return cmd
}

func newPXFFmtCmd(cli *CLI, defaultNS *string) *cobra.Command {
	var (
		schema  string
		msgName string
	)
	cmd := &cobra.Command{
		Use:   "fmt [namespace] <file.pxf>",
		Short: "Format PXF file using registry schema (stdout)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, file, err := nsAndFile(args, defaultNS)
			if err != nil {
				return err
			}
			desc, err := resolveRegistryDescriptor(cmd.Context(), cli.client, ns, schema, msgName)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(file) // #nosec G304 -- CLI-supplied input file path
			if err != nil {
				return err
			}
			msg, err := pxf.UnmarshalDescriptor(data, desc)
			if err != nil {
				return err
			}
			out, err := pxf.MarshalOptions{TypeURL: msgName}.Marshal(msg)
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(out)
			return err
		},
	}
	cmd.Flags().StringVar(&schema, "schema", "", "schema name (required)")
	cmd.Flags().StringVarP(&msgName, "message", "m", "", "fully qualified message name (required)")
	_ = cmd.MarkFlagRequired("schema")
	_ = cmd.MarkFlagRequired("message")
	return cmd
}

func nsAndFile(args []string, defaultNS *string) (string, string, error) {
	if len(args) == 2 {
		return args[0], args[1], nil
	}
	if defaultNS != nil && *defaultNS != "" {
		return *defaultNS, args[0], nil
	}
	return "", "", fmt.Errorf("namespace required: pass as first arg or use --namespace")
}
