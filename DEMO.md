# Local end-to-end demo — verify the Telegram DM

This drives the **whole pipeline** against a localhost mock instead of real
tiket.com, so you can confirm a DM actually arrives when an event flips
SOLD_OUT → AVAILABLE. Everything runs locally (no Docker needed for the app).

## Prerequisites
- A Telegram bot token from [@BotFather](https://t.me/BotFather).
- A reachable Postgres. Quickest:
  ```bash
  docker run -d --name tikom-pg -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=tikom \
    -p 5432:5432 postgres:16-alpine
  ```
- A local Chrome/Chromium (macOS: Google Chrome is fine).

## 1. Secrets
```bash
cp .env.example .env
```
Edit `.env`:
```
TELEGRAM_BOT_TOKEN=<your bot token>
DATABASE_URL=postgres://postgres:postgres@localhost:5432/tikom?sslmode=disable
# ADMIN_CHAT_ID can stay blank for the demo
```
`go run` auto-loads `.env` (via godotenv).

## 2. Start the mock site (terminal A)
```bash
go run ./cmd/mocksite
```
It serves on `:8099`, seeding `mock-bts-day1` as **sold out**. Open the admin
panel: <http://localhost:8099/admin>.

## 3. Start the watcher against the demo config (terminal B)
```bash
# macOS: point at local Chrome so it doesn't try to download Chromium
export CHROME_BIN="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
CONFIG_PATH=config.demo.yaml go run ./cmd/watcher
```
`config.demo.yaml` watches `http://localhost:8099/to-do/mock-bts-day1` with a
10s interval / 2s confirm / 1-minute cooldown. You'll see logs like
`checked event=mock-bts-day1 state=SOLD_OUT` once the baseline is recorded.

## 4. Subscribe (Telegram)
DM your bot **`/start`**. The watcher logs `subscriber upserted` and you get the
welcome message. (This stores your `chat_id` in Postgres so Broadcast can reach you.)

## 5. Flip it and get the DM
On the admin panel click **“→ AVAILABLE”** for `mock-bts-day1` (or:
`curl "http://localhost:8099/set?slug=mock-bts-day1&state=available"`).

Within ~10s the watcher logs:
```
confirm-before-alert event=mock-bts-day1 first=AVAILABLE confirm=AVAILABLE
ALERT sent: tickets available event=mock-bts-day1 subscribers=1
```
and you receive the DM:
> 🎟️ Tickets available again!
> MOCK: BTS Jakarta Day 1
> http://localhost:8099/to-do/mock-bts-day1

## 6. Repeat
Flip back to **SOLD_OUT**, wait past the 1-minute cooldown, flip to AVAILABLE
again to see another alert. Flipping AVAILABLE→AVAILABLE does nothing (no edge);
re-alerting within the cooldown is suppressed (logged as
`drop confirmed but within cooldown`).

## What this proves
- Headless render of a tiket-like page → eager detection (incl. ignoring the
  `<span>"Terjual habis"</span>` category badge that's present even when available).
- The edge-triggered `SOLD_OUT → AVAILABLE` transition with confirm-before-alert.
- The Telegram bot delivering the DM to a real subscriber.

## Cleanup
```bash
# Ctrl-C terminals A and B
docker rm -f tikom-pg
```

> Want to run it all in Docker instead? Run mocksite on the host, set the event
> URL in a config to `http://host.docker.internal:8099/to-do/mock-bts-day1`, and
> mount that config into the app container.
