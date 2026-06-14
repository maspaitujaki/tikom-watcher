# syntax=docker/dockerfile:1

# ---------- build stage ----------
FROM golang:1.26-bookworm AS build
WORKDIR /src

# Cache modules first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Fully static binary (all deps are pure Go) so it runs on the slim runtime image.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/watcher ./cmd/watcher

# ---------- runtime stage ----------
# headless-shell ships a headless Chromium at /headless-shell/headless-shell.
# Pin to a specific tag in production instead of :latest.
FROM chromedp/headless-shell:latest

# CA certificates for outbound HTTPS (Telegram API + tiket.com). tzdata is
# embedded in the binary via `_ "time/tzdata"`.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/watcher /usr/local/bin/watcher

ENV CHROME_BIN=/headless-shell/headless-shell \
    CONFIG_PATH=/app/config.yaml

WORKDIR /app
# config.yaml is mounted at runtime (see docker-compose.yml).
ENTRYPOINT ["/usr/local/bin/watcher"]
