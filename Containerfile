# Pinned to the multi-arch index digest of golang:1.27.4-alpine; update tag and digest together.
# The build stage runs on the build machine and cross-compiles, so no target emulation is needed.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
ARG TARGETOS TARGETARCH VERSION=devel
WORKDIR /src
COPY go.mod *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /justserve .

# Only the static binary: no shell, no libc, no TLS library.
FROM scratch
ARG VERSION=devel
LABEL org.opencontainers.image.title="justserve" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/belohnung/justserve" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /justserve /justserve
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/justserve"]
