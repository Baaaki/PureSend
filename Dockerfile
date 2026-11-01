# Image for the rendezvous + relay server (cmd/server).
#
# This is the ONLY part of the project that runs on your Ubuntu box. The
# client (cmd/client) is a desktop app users download — never run it in a
# container, the extra NAT layer breaks hole punching.

# Stage 1: build. The Go version matches the toolchain line in go.mod; CI
# and the release build use the same one. Pinned to the patch release so
# a builder with an older image cached still gets the fixes.
FROM golang:1.27.3-alpine3.24 AS build
WORKDIR /app

# Download dependencies first so this layer is cached across code changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Version information is stamped in so `server -version` and the /health
# and /metrics endpoints can say which build is running — the first thing
# anyone asks when a deployment misbehaves.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
      -o /server ./cmd/server

# Stage 2: runtime — a small image containing only the binary
FROM alpine:3.24
WORKDIR /data
COPY --from=build /server /usr/local/bin/server

# The identity key is written to /data/server.key. Mount a volume here:
# if the peer ID changes, every client already released stops working.
VOLUME /data

# 8080 = libp2p WebSocket listener; this is what Cloudflare Tunnel targets.
# 8081 = /health for the supervisor (OpenShip) to probe, and /metrics for
#        Prometheus. Keep 8081 off the public internet.
EXPOSE 8080 8081

# The image checks itself, rather than relying on whoever runs it to copy
# the check over: a platform that honours only part of a compose file
# (OpenShip drops several blocks) still sees the container's health.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8081/health >/dev/null || exit 1

# Nothing here needs root, and the identity key is the one secret in the
# image's world. USER comes after the VOLUME so /data is owned correctly.
RUN adduser -D -u 10001 ft && chown ft /data
USER ft

ENTRYPOINT ["server"]
# The health endpoint listens on every interface *inside* the container —
# the server binds it to loopback by default, which a port mapping could
# not reach. What the host exposes is up to the compose file, which keeps
# it on 127.0.0.1.
CMD ["-ws-port", "8080", "-health-addr", "0.0.0.0:8081", "-key", "/data/server.key"]
