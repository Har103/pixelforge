# ---------------------------------------------------------------- build ------
# Pinned to an exact patch release, not a floating major. The image carries no
# OS packages and no third-party modules, so the Go toolchain is the entire
# attack surface of the shipped binary - every CVE a scanner can find here is a
# stdlib CVE, and the only lever is this line. Go 1.24 and earlier are
# end-of-life as of Go 1.26.
FROM golang:1.26.6-alpine AS build

RUN apk add --no-cache ca-certificates

WORKDIR /src

# go.mod is copied first so the dependency layer is cached independently of the
# source. It declares no requirements - the whole project is standard library -
# so this resolves instantly, but keeping the step means adding a dependency
# later does not silently change the build's shape.
COPY go.mod ./
RUN go mod download

COPY . .

ARG VERSION=dev

# CGO off and a static link so the result runs on an empty base image.
# -trimpath keeps build paths out of the binary; -s -w drop the symbol and DWARF
# tables, which is most of the size saving.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/pixelforge ./cmd/pixelforge

# Verify the tests pass inside the same environment that produced the binary.
RUN go vet ./... && go test ./...

# A minimal passwd entry so the final image can run as a non-root user even
# though there is no distribution underneath it to provide one.
RUN echo 'pixelforge:x:65532:65532:pixelforge:/:/sbin/nologin' > /out/passwd

# ---------------------------------------------------------------- runtime ----
FROM scratch

# Root certificates, so the PostgreSQL driver can verify a managed database's
# TLS chain when sslmode is verify-ca or verify-full.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/passwd /etc/passwd
COPY --from=build /out/pixelforge /pixelforge

USER pixelforge

ENV PORT=8080 \
    LOG_FORMAT=json \
    CANVAS_WIDTH=256 \
    CANVAS_HEIGHT=256 \
    COOLDOWN_MS=750

EXPOSE 8080

# No shell in this image, so the binary probes itself.
HEALTHCHECK --interval=20s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/pixelforge", "-healthcheck"]

ENTRYPOINT ["/pixelforge"]
