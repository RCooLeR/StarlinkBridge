# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine3.24@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/starlink-bridge .

FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
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
