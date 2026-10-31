# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine3.24@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/starlink-bridge .

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 bridge \
    && adduser -S -D -H -u 10001 -G bridge bridge \
    && mkdir -p /app/config /app/data \
    && chown -R bridge:bridge /app
COPY --from=builder --chmod=0755 /out/starlink-bridge /usr/bin/starlink-bridge
USER 10001:10001
WORKDIR /app
EXPOSE 9091
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/usr/bin/starlink-bridge", "healthcheck"]
ENTRYPOINT ["/usr/bin/starlink-bridge"]
CMD ["serve"]
