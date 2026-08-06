# One static binary in one image. CGO_ENABLED=0 with no cgo dependencies (D11)
# means the runtime stage needs nothing underneath the binary, so it is scratch.
#
# When SQLite lands the driver stays pure Go (modernc.org/sqlite), so this stays
# scratch. Litestream becomes the entrypoint wrapping the binary at that point;
# it is a static Go binary too and copies into this image the same way.

FROM golang:1.23-alpine AS build

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

# Numeric because scratch has no /etc/passwd. The bind-mounted data directory
# must be writable by this uid — see the note in docker-compose.yml.
USER 65532:65532

EXPOSE 8000

# No HEALTHCHECK: it would run inside a container with no shell and no curl.
# /healthz is checked from outside, by Prometheus and by whoever is looking.
ENTRYPOINT ["/navi"]
