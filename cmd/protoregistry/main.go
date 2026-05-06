// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"os"

	"github.com/trendvidia/protoregistry/internal/ctl"
)

func main() {
	if err := ctl.NewRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
