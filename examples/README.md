# Examples

Runnable programs that exercise the gRPC integration point of the
`protoregistry` module.

| Example                          | Side     | What it shows                                                                                                                                                                                                                  |
|----------------------------------|----------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| [`client-grpc/`](client-grpc/)   | Producer | Talk to a running `protoregistry serve` over the raw gRPC API: create a namespace, publish a schema, promote it, fetch the compiled descriptor back. Mirrors what the CLI does end-to-end in ~80 lines.                        |
| [`client-sdk/`](client-sdk/)     | Consumer | Use the [`client/`](../client/) Go SDK to resolve message types by fully-qualified name, build a `dynamicpb.Message`, round-trip it through `protojson`, and (optionally) derive a version-pinned resolver for replayable reads. |

`cd` into the directory and run `go run .`. Each example assumes a server
reachable at `localhost:50051` (override with `--addr`).

Run them in order — the SDK example expects a schema to already be
published in the target namespace, which `client-grpc/` is what produces.

## Embedding the library directly

For the in-process library API (no gRPC), see the
[*Using as a Go library*](../README.md#using-as-a-go-library) section
of the main README — it's a complete runnable snippet that requires
PostgreSQL but no extra example scaffolding.

## Bootstrapping built-in types

The third documented entry point — loading shared types from a directory
at server startup — is a one-liner:

```bash
protoregistry serve --db "$DATABASE_URL" --migrate --builtins ./company-types/
```

See the [*Built-in Types*](../README.md#built-in-types) section of the
main README for the resolution-order details.
