# syntax=docker/dockerfile:1.7
#
# Multi-stage, multi-arch build for the Zot cache warmer.
# Build with: docker buildx build --platform linux/amd64,linux/arm64 -t zot-cache-warmer:latest .
#
# Stage 1 runs on the host (BUILDPLATFORM) and cross-compiles for the requested
# TARGETPLATFORM. CGO is disabled so the binary is statically linked and lands
# cleanly in distroless/static.
#
# NOTE: the Go base image major.minor MUST be >= the `go` directive in go.mod.
# When go.mod's Go version is bumped (typically by `go mod tidy` on a newer
# local toolchain), bump this tag in lockstep. A lower base version will fail
# inside buildx with `go mod download` exit 1 when Go's auto-toolchain
# download path hits the network constraints of the build container.
#
# This image ships ONLY the cluster CronJob binary (cmd/zot-cache-warmer).
# The standalone CLI (cmd/zot-warm) is published separately as a release
# binary — see .github/workflows/release.yaml.

FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

# buildx-provided variables. TARGETOS/TARGETARCH/TARGETVARIANT are populated
# automatically by Docker Buildx from the --platform list.
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=dev

WORKDIR /build

# Prime the module cache before copying the full tree so changes to source
# files don't invalidate the dependency download layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Strip symbols (-s -w) for a smaller binary; embed Version via ldflags.
# Build path targets the cluster binary explicitly. The standalone CLI at
# cmd/zot-warm is not compiled into this image — it is published as a
# standalone release artifact from a separate workflow job.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    go build \
        -trimpath \
        -ldflags="-s -w -X main.Version=${VERSION}" \
        -o /out/zot-cache-warmer \
        ./cmd/zot-cache-warmer

# Stage 2: distroless static ships CA certificates at
# /etc/ssl/certs/ca-certificates.crt, which Go's x509.SystemCertPool() reads
# automatically on Linux. The :nonroot tag runs as UID 65532 so the container
# satisfies Pod Security Standards "restricted" without additional config.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/zot-cache-warmer /zot-cache-warmer

ENTRYPOINT ["/zot-cache-warmer"]
