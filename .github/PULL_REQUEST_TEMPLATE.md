<!--
Thanks for contributing! A few notes before you open this PR:

- Open an issue first for non-trivial changes so we can agree on the approach.
- Keep one logical change per PR; refactors and feature work should not mix.
- Run `make vet test-unit headers-check lint` locally before pushing.
-->

## Summary

<!-- What does this change do, and why? Link related issues with `Fixes #N`. -->

## Public surface impact

<!-- Tick all that apply. See STABILITY.md for what is considered breaking. -->

- [ ] gRPC service (`proto/protoregistry/v1/`)
- [ ] Go library (exported API)
- [ ] Go client SDK
- [ ] CLI flags / output
- [ ] Database schema / migrations
- [ ] None of the above (internal refactor, tests, docs)

## Tests

<!-- Which tests cover this change? `make test` for the full suite, `make test-unit` for the no-Docker subset. -->

## Checklist

- [ ] Tests added or updated
- [ ] `CHANGELOG.md` updated under `## Unreleased` (for user-visible changes)
- [ ] SPDX header on any new `.go` files (`make headers`)
- [ ] No breaking change, **or** breaking change is called out above and documented in the changelog
