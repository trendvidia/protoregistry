# syntax=docker/dockerfile:1.7

FROM golang:1.26-alpine AS build
WORKDIR /src

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/protoregistry ./cmd/protoregistry

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/protoregistry /usr/local/bin/protoregistry
EXPOSE 50051
ENTRYPOINT ["/usr/local/bin/protoregistry"]
CMD ["serve", "--listen", ":50051", "--migrate"]
