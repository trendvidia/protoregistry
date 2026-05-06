// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package migrations embeds the SQL migration files for use by goose.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
