# One static binary in one image. CGO_ENABLED=0 with no cgo dependencies (D11)
# means the runtime stage needs nothing underneath the binary, so it is scratch.
#
# SQLite is here now and the driver is pure Go (modernc.org/sqlite), so this is
# still scratch and the binary is still static. Litestream (session 7) is the
# entrypoint wrapping the binary: it is itself a static Go binary (verified —
# `file` reports ELF 64-bit, statically linked, for the pinned tag below), so
# it copies into scratch the same way navi does.
#
# The builder tracks the go directive in go.mod, which modernc.org/sqlite raised
# to 1.25.

FROM litestream/litestream:0.3.13 AS litestream

FROM golang:1.25-alpine AS build

RUN apk add --no-cache ca-certificates

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# -trimpath keeps build paths out of the binary; -s -w drops the symbol table
# and DWARF, which this deployment has no use for.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/navi ./cmd/navi


FROM scratch

# Nothing dials out yet. The bundle is here because the session that adds the
# Telegram client would otherwise spend its first hour on an x509 error in a
# container with no shell to debug it.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=build /out/navi /navi

# Litestream wraps navi as the container entrypoint (03-architecture.md,
# Deployment) rather than running as a second compose service — D5 permits a
# separate service only when it is *not* run as entrypoint, and one process to
# supervise is simpler than two. Its own config, with everything that varies
# left as env-var substitutions, is baked in the same way config/ is (see
# ops/litestream/litestream.yml for why this file doesn't need the bind-mount
# treatment config/ gets).
COPY --from=litestream /usr/local/bin/litestream /litestream
COPY --from=build /src/ops/litestream/litestream.yml /etc/litestream.yml

# The vocabulary table, and later the persona, travel in the image so a
# forgotten mount is a stale table rather than a process that will not start.
# The compose file bind-mounts ./config over this, which is what makes retuning
# an edit and a restart instead of a rebuild (D-016, G5).
COPY --from=build /src/config /config

# Numeric because scratch has no /etc/passwd. The bind-mounted data directory
# must be writable by this uid — see the note in docker-compose.yml.
USER 65532:65532

EXPOSE 8000
# litestream's own metrics, scraped by Prometheus over the container network
# the same way navi:8000/metrics is — never through the tunnel.
EXPOSE 9200

# No HEALTHCHECK: it would run inside a container with no shell and no curl.
# /healthz is checked from outside, by Prometheus and by whoever is looking.
#
# -exec starts /navi as a subprocess and forwards SIGTERM to it, which is what
# keeps D12's shutdown ordering (HTTP drains, then loops cancel, then the
# process exits) intact under this wrapper rather than assumed — verified in
# the mid-day restart drill (ops/restore-runbook.md).
ENTRYPOINT ["/litestream", "replicate", "-exec", "/navi", "-config", "/etc/litestream.yml"]
