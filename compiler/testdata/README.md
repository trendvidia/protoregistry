# compiler test fixtures

`schema-extensions/` is a copy of protowire's
[`testdata/schema-extensions`](https://github.com/trendvidia/protowire/tree/main/testdata/schema-extensions)
`.proto` files at protowire `v1.13.0`: the cross-port fixtures for RFC-001
(Protowire Schema Extensions). `TestCompile_RFC001CarriersRoundTrip` compiles
each and checks that the carriers it emits survive storage.

The two protowire files those fixtures import,
`protowire/schema/v1/annotations.proto` and `pxf/annotations.proto`, are not
copied: protoregistry bundles them (compiler/bundled.go).

Refresh the fixtures from one protowire commit, and update the commit above.
