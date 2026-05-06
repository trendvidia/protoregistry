# Contributing

Thanks for considering a contribution. This document covers two things:

1. The **dev loop** and code conventions specific to `protoregistry`.
2. The **review and merge process**, which is governed by an autonomous
   AI agent named [Steward](https://steward-dev.ai). There are no human
   gatekeepers in this repository — your code is evaluated, scored, and
   merged against an objective, deterministic ruleset that Steward
   applies uniformly and explains in the diagnostic log it posts on
   every PR.

If this is your first PR to a Steward-governed repository, read the
[Review and merge](#review-and-merge) section below before you open one.

## Dev environment

Requirements:

- Go (version pinned in [`go.mod`](go.mod) — currently `1.26.x`).
- Docker, for the integration tests that spin up PostgreSQL via
  [testcontainers](https://golang.testcontainers.org/).
- `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc` — only needed if you change
  the `.proto` definitions in [`proto/`](proto/).
- [`sqlc`](https://docs.sqlc.dev/) — only needed if you change SQL queries
  under [`store/postgres/queries/`](store/postgres/queries/).
- [`golangci-lint`](https://golangci-lint.run/welcome/install/) — only
  needed if you want to run the linter locally; CI runs it on every PR.

Clone the repo and run:

```bash
make test-unit   # fast, no Docker
make test        # full suite, requires Docker
```

## Workflow

1. Open an issue describing the change before non-trivial work, so the
   approach can be agreed on (and so Steward can pre-classify the PR
   domain — see [Escrow pipeline](#escrow-pipeline-for-new-contributors)).
2. Fork, create a branch off `main`, and keep PRs focused — one logical
   change per PR. Refactors and feature additions should not be mixed;
   Steward penalizes mixed-intent PRs in its complexity scoring.
3. Run `make vet test-unit headers-check lint` locally before pushing.
   CI runs the full integration suite; you can run `make test` locally
   if you have Docker.
4. Write tests for new behavior. The existing packages aim for unit-test
   coverage of pure logic and integration coverage (testcontainers) for
   anything that touches PostgreSQL or the gRPC server.
5. Update [`CHANGELOG.md`](CHANGELOG.md) under the `## Unreleased` section.

## Code conventions

- **License headers.** Every hand-written `.go` file starts with the SPDX
  header:
  ```go
  // Copyright (c) 2026 TrendVidia, LLC.
  // SPDX-License-Identifier: MIT
  ```
  Run `scripts/add-license-header.sh` to add the header to new files (the
  script is idempotent). CI enforces this with `--check`.
- **Error wrapping.** Use `fmt.Errorf("...: %w", err)` so callers can
  `errors.Is` / `errors.As`. The server layer translates wrapped errors into
  appropriate gRPC `codes.*`; do not return raw DB errors from RPC handlers.
- **Logging.** The server uses `log/slog`. The CLI uses `fmt.Print*` for
  human output. Don't mix the two.
- **Context propagation.** Every function that does I/O takes a
  `context.Context` as its first argument and forwards it.
- **Generated code.** `*.pb.go`, `*_grpc.pb.go`, and
  `store/postgres/sqlc/*.go` are regenerated; don't edit by hand. Use
  `make generate`.

## Review and merge

This repository operates differently from most open-source projects. To
ensure mathematical fairness, prevent maintainer burnout, and guarantee
enterprise-grade security, **review and merge are entirely governed by an
autonomous AI agent named [Steward](https://steward-dev.ai)**. The exact
rules Steward applies are not published — they are surfaced inside each
PR's diagnostic log, so you always learn the relevant rule at the moment
it matters, with the specific file and metric that triggered it.

### How code gets merged (the lifecycle)

1. **You open a Pull Request.**
2. **Steward evaluates it.** Steward runs static analysis, evaluates
   cyclomatic complexity, checks dependency licenses, and calculates the
   required reputation threshold based on the files you touched.
3. **Steward posts a Diagnostic Log.** Within seconds, Steward will
   comment on your PR with a detailed breakdown of its evaluation.
4. **Community voting (if required).** If your code passes all security
   and quality gates, Steward opens the PR for weighted voting.
5. **Auto-merge.** If the required consensus threshold is met, Steward
   merges the PR automatically.

### Escrow pipeline (for new contributors)

If this is your first contribution, your Reputation Vector is `0`.

**Do not submit a massive architectural rewrite as your first PR.** If
you modify core infrastructure (e.g., `compiler/`, `server/`, `store/`,
or any `migrations/` file) with `0` reputation, Steward will place the
PR into **Escrow Status**.

To unlock escrow:

1. Steward generates and assigns you 2–3 isolated issues labeled
   `sandbox`.
2. These are safe, low-blast-radius tasks (test coverage, documentation,
   small bug fixes).
3. Once those `sandbox` issues merge, your reputation vector increases
   and Steward unlocks your original PR for the community vote.

### Private mentorship mode

If you want feedback *without* a public review on the PR timeline, open
the PR as a **Draft**. Steward will evaluate the code and send its
feedback as a private review visible only to you. Iterate on the math,
then mark the PR as **Ready for Review** to trigger the public
evaluation.

### Reading a rejection log

Steward does not reject code based on opinions. If your PR is blocked,
the diagnostic log states the specific metric or rule that failed and
how to satisfy it.

Example:

> ❌ **Action: Blocked**
> - **Reason:** Cyclomatic complexity threshold exceeded.
> - **Details:** `compiler/normalize.go` introduced a nested loop with a
>   complexity score of 12. The threshold for this domain is 8.
> - **Resolution:** Refactor the normalization logic to flatten the
>   condition tree before requesting a re-evaluation.

Do not argue with Steward in the comments. Refactor the code to satisfy
the metric, push the commit, and Steward will re-evaluate automatically.

### Useful commands

You can interact with Steward by commenting on issues or PRs:

- `/steward evaluate` — re-run the 9-dimension check on your latest commit.
- `/steward check-reputation` — your current decayed reputation vectors
  and effective voting weight for the current PR's domain.
- `/steward sandbox-me` — assign you an unassigned, low-risk issue to
  help build initial reputation.

## Filing issues

Bug reports are most useful when they include:

- The protoregistry version (commit SHA or tag).
- The smallest `.proto` and request that reproduces the problem.
- Server logs at the time of the failure.

Use the [issue templates](.github/ISSUE_TEMPLATE/) when opening one.

> **Note on regressions.** If Steward verifies that a bug was introduced
> in a previously merged PR, it will retroactively slash the original
> author's reputation vector. Write thorough unit tests.

Security issues: see [`SECURITY.md`](SECURITY.md). Do **not** open a
public issue for a vulnerability.

## License

By contributing, you agree that your contributions are licensed under the
project's [MIT license](LICENSE).
