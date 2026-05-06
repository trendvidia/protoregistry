#!/usr/bin/env bash
# Adds the SPDX license header to every hand-written Go file in the repo.
# Idempotent: files that already start with the SPDX line are left alone.
# Generated files (*.pb.go, *_grpc.pb.go, store/postgres/sqlc/*.go) are skipped
# since they are regenerated from proto / SQL sources.
#
# Usage:
#   scripts/add-license-header.sh           # add headers in place
#   scripts/add-license-header.sh --check   # exit non-zero if any file is missing the header

set -euo pipefail

HEADER='// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT
'

MARKER='SPDX-License-Identifier: MIT'

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

mode="apply"
if [[ "${1:-}" == "--check" ]]; then
    mode="check"
fi

# Collect candidate files.
mapfile -t files < <(
    find . -type f -name '*.go' \
        -not -path './.git/*' \
        -not -name '*.pb.go' \
        -not -name '*_grpc.pb.go' \
        -not -path '*/store/postgres/sqlc/*' \
        | sort
)

missing=()
for f in "${files[@]}"; do
    if grep -q "$MARKER" "$f"; then
        continue
    fi
    if [[ "$mode" == "check" ]]; then
        missing+=("$f")
        continue
    fi
    tmp="$(mktemp)"
    printf '%s\n' "$HEADER" > "$tmp"
    cat "$f" >> "$tmp"
    mv "$tmp" "$f"
    echo "added: $f"
done

if [[ "$mode" == "check" ]]; then
    if (( ${#missing[@]} > 0 )); then
        printf 'Missing SPDX header in %d file(s):\n' "${#missing[@]}" >&2
        printf '  %s\n' "${missing[@]}" >&2
        exit 1
    fi
    echo "All Go files carry the SPDX header."
fi
