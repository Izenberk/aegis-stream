# --- Stage 1: Build ---
# Uses the full Go toolchain image to compile.
# This image is ~800MB — we only use it for building, never ship it.
FROM golang:1.24-alpine AS builder

# Install ca-certificates so the final binary can make HTTPS calls if needed.
# Alpine uses apk instead of apt.
RUN apk add --no-cache ca-certificates

WORKDIR /src

# Copy go.mod and go.sum first, then download dependencies.
# Docker caches each layer. If go.mod hasn't changed, this layer is reused
# and `go mod download` is skipped — saves minutes on rebuilds.
COPY go.mod go.sum ./
RUN go mod download

# Now copy the rest of the source code.
# This layer only rebuilds when source files change.
COPY . .

# Build a statically linked binary:
#   CGO_ENABLED=0  — no C dependencies, pure Go (required for scratch image)
#   -ldflags="-s -w" — strip debug info and symbol tables (smaller binary)
#   -o /bin/server  — output path inside the builder container
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /bin/server ./cmd/server

# --- Stage 2: Runtime ---
# scratch is a completely empty image — no OS, no shell, no tools.
# The only thing in it will be our binary and CA certs.
# This is why the binary must be statically linked (CGO_ENABLED=0).
FROM scratch

# Copy CA certificates so the binary can verify TLS (needed for HTTPS metrics scraping, etc.)
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy just the compiled binary from the builder stage.
# Everything else (Go toolchain, source code, dependencies) is discarded.
COPY --from=builder /bin/server /server

# Document which ports this container uses.
# EXPOSE doesn't publish ports — it's documentation for K8s manifests.
EXPOSE 9000 2112

# The entrypoint is the binary itself. No shell needed.
ENTRYPOINT ["/server"]
