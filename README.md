# tikom — tiket.com ticket-drop notifier

A self-hosted Go service that watches sold-out concert pages on
[tiket.com](https://www.tiket.com) with a **headless browser** and DMs Telegram
subscribers the moment a sold-out event's buy button becomes available again.

- **Eager detection** — prefers a false alarm over missing a real drop.
- **Config-owned watch list** — events live in a committed `config.yaml`; the DB
  holds only runtime state + subscribers. No Telegram admin commands.
- **Headless-browser based** — renders the page and inspects the buy button
  (not the underlying availability API).

## How it works

Every `interval ± jitter` the poller renders each configured event and classifies it:

| Result | Meaning |
|---|---|
| `UNKNOWN` | purchase section never rendered / challenge / load failure (never alerts) |
| `SOLD_OUT` | the primary purchase `<button>` is disabled and reads "Terjual habis" |
| `AVAILABLE` | purchase section rendered and no sold-out button matched (eager default) |

Notifications fire **only** on the stored `SOLD_OUT → AVAILABLE` edge, with:

- **Confirm-before-alert** — on a flip, re-check once after `confirm_delay`; the
  re-check is authoritative, filtering single-render glitches without delaying real drops.
- **Cooldown** — no re-alert for the same event within `cooldown_minutes` (flapping).
- **Unknown-streak warning** — after `unknown_streak_threshold` consecutive
  `UNKNOWN`s, the admin is DM'd "site may have changed / blocked".

Identity is the stable `event_key`, so changing an event's URL/slug never resets its history.

## Layout

```
cmd/watcher      production entrypoint (config + DB + bot + poller, graceful shutdown)
cmd/bot          Phase-2 bot smoke test (in-memory store)
cmd/spike        Phase-0 throwaway: dump rendered HTML/screenshots for a URL
internal/detect  pure classification (Classify) + config-driven Rules
internal/browser one reused headless Chrome: per-check timeout, crash recovery, retry/backoff
internal/notify  Telegram long-polling bot + SubscriberStore + Broadcast
internal/poll    the orchestration state machine (interfaces + edge logic)
internal/postgres pgx pool, goose embedded migrations, SubscriberStore + EventStore
internal/config  YAML loader (poll timing, active hours, detection rules, events)
testdata/fixtures saved tiket.com HTML used by detection tests
```

## Prerequisites

- Docker + Docker Compose (recommended), **or** Go 1.25+, a Chrome/Chromium, and Postgres for local dev.
- A Telegram bot token from [@BotFather](https://t.me/BotFather).

## Setup

```bash
cp .env.example .env        # fill in TELEGRAM_BOT_TOKEN, POSTGRES_PASSWORD, etc.
# edit config.yaml to set the events you want to watch
```

- `config.yaml` (committed, no secrets): poll timing, optional `active_hours`,
  browser settings, detection rules, and the **events** list.
- `.env` (gitignored): `TELEGRAM_BOT_TOKEN`, `ADMIN_CHAT_ID` (optional),
  `POSTGRES_*`, and `DATABASE_URL` (for non-docker runs).

To find your `ADMIN_CHAT_ID`, DM [@userinfobot](https://t.me/userinfobot).

## Run with Docker Compose

```bash
docker compose up -d --build
docker compose logs -f app
```

This starts Postgres (named volume `pgdata`, healthchecked) and the app
(`depends_on: service_healthy`, `restart: unless-stopped`). Migrations run
automatically on startup. The runtime image is `chromedp/headless-shell`; Chrome
runs with `--no-sandbox --disable-dev-shm-usage` and the service gets `shm_size: 1gb`.

Then, in Telegram, DM your bot:

- `/start` — subscribe (you'll get the drop alerts)
- `/stop` — unsubscribe

## Local development

```bash
go test ./...                 # unit tests (Postgres + live-browser tests auto-skip)

# Full suite incl. Postgres integration:
docker run -d --name pg -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=tikom -p 5432:5432 postgres:16-alpine
TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/tikom?sslmode=disable' go test ./...

# Live end-to-end detection against real tiket.com (needs internet + Chrome):
TIKOM_LIVE=1 go test ./internal/browser/ -run Live -v

# Run the app locally (needs a reachable Postgres + a bot token in .env):
go run ./cmd/watcher

# Inspect a page's rendered DOM/screenshot (Phase-0 spike):
go run ./cmd/spike
```

## Adding / changing watched events

Edit the `events:` list in `config.yaml` and restart. Adding an event "just
works" — the app upserts it (`ON CONFLICT (event_key) DO NOTHING`). Use a stable,
unique `key`; you can change `name`/`url` later without losing history.

A per-event `detection:` block overrides the `detection_defaults` (e.g. a
different `sold_out.text_any` or `page_ready_selector`).

## Inspecting state

```bash
docker compose exec postgres psql -U tikom -d tikom \
  -c "SELECT event_key, last_state, unknown_streak, last_changed_at, last_notified_at FROM event_state;"
docker compose exec postgres psql -U tikom -d tikom \
  -c "SELECT * FROM state_transitions ORDER BY observed_at DESC LIMIT 20;"
```
