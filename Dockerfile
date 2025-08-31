# ---------- Builder ----------
FROM golang:1.22-alpine AS build
RUN apk add --no-cache ca-certificates
WORKDIR /src

# Cache deps
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Build
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/scanner ./


FROM scratch
# TLS roots for HTTPS (needed by go-git)
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Binary
COPY --from=build /out/scanner /bin/scanner

# Workdir is where we write ./sample-keys-repo and its .scan_state.json
WORKDIR /data

USER 65532:65532

ENTRYPOINT ["/bin/scanner"]
