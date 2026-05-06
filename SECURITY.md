# Security policy

## Supported versions

protoregistry is currently in the **`v0.x`** pre-stable line. Only the latest
minor release receives security fixes. There is no long-term support branch
yet — that will arrive with `v1.0`.

| Version | Supported          |
|---------|--------------------|
| `0.x` (latest minor) | yes |
| older `0.x`          | no  |

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security problems.

Send a report to **security@trendvidia.com** with:

- A description of the issue and the impact you observed.
- The smallest reproducer you can share (a `.proto` file, a gRPC payload,
  the relevant CLI invocation, etc.).
- The protoregistry version (commit SHA or tag) and Go version you tested
  against.
- Whether you would like to be credited in the fix announcement.

We will acknowledge your report within **3 business days** and aim to ship a
fix or mitigation within **30 days** for high-severity issues. We will keep
you informed of progress and coordinate disclosure timing with you.

## Threat model and known limitations

Operators deploying protoregistry should be aware of the following:

- **The gRPC server has no built-in authentication.** Bind it only to trusted
  networks, or place it behind a reverse proxy / service mesh that enforces
  mTLS or token auth. A pluggable authentication interceptor is on the
  roadmap; until it lands, treat the server as administrator-only.
- **`.proto` compilation runs untrusted input through `protocompile`.** Even
  with the input-size and timeout guards we apply, malicious schemas can
  consume non-trivial CPU and memory. Do not allow anonymous publishes from
  untrusted callers.
- **The `__builtins__` namespace is privileged.** Files published there are
  visible to every other namespace and can shadow well-known types (with
  `--force`). Restrict write access to it at the network/auth layer.
- **PostgreSQL credentials** are read from `--db` / `DATABASE_URL`. Avoid
  passing them on the command line in shared shells; prefer the environment
  variable or a secrets file.

These limitations are tracked in [`STABILITY.md`](STABILITY.md) and in open
issues. Reports that materially extend or contradict the list above are
welcome via the disclosure channel above.
