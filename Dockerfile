# ============================================================
# thefeed-server — Multi-stage Docker build
# ============================================================
# Build:  docker compose build
# Run:    docker compose up -d
# Login:  docker compose run -it --rm server --login-only \
#           --data-dir /data --domain $THEFEED_DOMAIN \
#           --key $THEFEED_KEY --api-id $TELEGRAM_API_ID \
#           --api-hash $TELEGRAM_API_HASH --phone $TELEGRAM_PHONE
# ============================================================

# ---- Stage 1: Build ----
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build-time version info (overridable via --build-arg)
ARG VERSION=docker
ARG COMMIT=unknown
ARG DATE=unknown

RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w \
      -X github.com/sartoopjj/thefeed/internal/version.Version=${VERSION} \
      -X github.com/sartoopjj/thefeed/internal/version.Commit=${COMMIT} \
      -X github.com/sartoopjj/thefeed/internal/version.Date=${DATE}" \
    -o /thefeed-server ./cmd/server

# ---- Stage 2: Runtime ----
FROM alpine:3.21

# Copy ca-certificates and tzdata from builder (avoids second apk fetch
# which can fail on restricted networks).
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Non-root user for security
RUN adduser -D -u 1000 -h /data thefeed

COPY --from=builder /thefeed-server /usr/local/bin/thefeed-server

# Data directory: channels.txt, x_accounts.txt, session.json, cache
VOLUME /data

# DNS listen port (mapped to host:53 via docker-compose)
EXPOSE 5300/udp

USER thefeed
WORKDIR /data

ENTRYPOINT ["thefeed-server"]
CMD ["--data-dir", "/data", "--listen", ":5300"]
