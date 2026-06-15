# ── Build stage ────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG GIT_HASH=""
RUN GIT_HASH="${GIT_HASH:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}" && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w -X main.version=${GIT_HASH}" -o /bin/splitclicker ./cmd/server

# ── Runtime stage ───────────────────────────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache ca-certificates
COPY --from=builder /bin/splitclicker /splitclicker
# goose reads migrations from the filesystem at startup, so ship them alongside.
COPY --from=builder /src/migrations /migrations
# Host-editable runtime dir: skin images + config.json (timestamp/skin selection),
# served off disk at GET /api/v1/skin and /api/v1/config. Baked in as a baseline;
# the host bind-mounts its own ./data over /data to edit these live (see compose).
COPY --from=builder /src/data /data
ENV RUNTIME_DIR=/data

EXPOSE 6969
ENTRYPOINT ["/splitclicker"]
