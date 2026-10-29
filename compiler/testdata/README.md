# compiler test fixtures

`schema-extensions/` is a copy of protowire's
[`testdata/schema-extensions`](https://github.com/trendvidia/protowire/tree/main/testdata/schema-extensions)
`.proto` files at protowire `1ae3c2d`: the cross-port fixtures for RFC-001
(Protowire Schema Extensions). `TestCompile_RFC001CarriersRoundTrip` compiles
each and checks that the carriers it emits survive storage.

`protowire-builtins/` holds the one file a fixture imports that protoregistry
does not bundle, copied from protowire's `proto/` tree at the same commit: the
legacy `pxf/annotations.proto`. The test supplies it as `__builtins__`.
`protowire/schema/v1/annotations.proto` is bundled (compiler/bundled.go).

Refresh both together from one protowire commit, and update the commit above.
