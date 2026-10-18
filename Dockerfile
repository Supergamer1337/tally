# syntax=docker/dockerfile:1

# Build on the runner's own platform and cross compile, so arm64 images need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
ARG TARGETOS TARGETARCH VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/tally ./cmd/tally \
 && mkdir -p /out/data

# One static binary and the CA certificates it needs to reach a sign in provider over HTTPS.
# Time zones are built into the binary. For a provider with a private CA, mount its certificate
# and point SSL_CERT_FILE at it.
#
# The container starts as root only so tally can make /data its own, which a bind mount that
# Docker created for root is not. It then runs as PUID:PGID. Run it with `user:` and it starts as
# that user and leaves /data alone.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/tally /tally
COPY --from=build --chown=65532:65532 /out/data /data
ENV PUID=65532 PGID=65532
ENV TALLY_ADDR=:8080 TALLY_DB=/data/tally.db
EXPOSE 8080
VOLUME /data
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 CMD ["/tally", "healthcheck"]
ENTRYPOINT ["/tally"]
