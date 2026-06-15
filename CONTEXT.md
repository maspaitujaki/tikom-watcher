# CONTEXT

## 1. Purpose

`tikom` is a self-hosted Go service that watches concert/event ticket pages on
[tiket.com](https://www.tiket.com) and sends Telegram DMs when an event's
availability changes — primarily when a sold-out event's buy button comes back
(`SOLD_OUT → AVAILABLE`), and also when it sells out (`AVAILABLE → SOLD_OUT`).

It deliberately works by **rendering the page in a headless browser and
inspecting the buy button**, not by calling tiket.com's underlying availability
API. Detection is **eager**: when uncertain it prefers a false alarm over missing
a real drop, and a failed/blocked page load is always classified `UNKNOWN`
(never as available or sold out).

The watched-event list is **config-owned** (a committed YAML file). The database
holds only runtime state and subscribers; there are no Telegram admin commands to
add or remove events.

## 2. Architecture Overview

The system is a single long-running process (`cmd/watcher`) composed of small
packages wired together at startup. Each layer depends only on interfaces, so the
pieces are independently testable.

```
                 Telegram users                     tiket.com
                  │   ▲                                  ▲
          /start  │   │ DMs (alerts, /status)            │ HTTPS (headless Chrome)
          /stop   ▼   │                                  │
        ┌──────────────────┐   Broadcast/SendTo   ┌──────────────┐
        │ internal/notify  │◀─────────────────────│ internal/poll│
        │ (telebot bot)    │                       │ (Poller +    │
        │  SubscriberStore │                       │  state machine)
        └────────┬─────────┘                       └───┬───────┬──┘
                 │                          Detector    │       │ EventStore
                 │                  (RendererDetector)   │       │
                 │                                       ▼       │
                 │                          ┌────────────────┐   │
                 │                          │ internal/detect│   │
                 │                          │  Classify(html)│   │
                 │                          └───────┬────────┘   │
                 │                          Renderer │           │
                 │                                   ▼           │
                 │                          ┌────────────────┐   │
                 │                          │ internal/browser│  │
                 │                          │  (go-rod Chrome)│  │
                 │                          └────────────────┘   │
                 ▼                                               ▼
        ┌────────────────────────────────────────────────────────┐
        │ internal/postgres (pgxpool + goose migrations)           │
        │   subscribers · event_state · state_transitions          │
        └────────────────────────────────────────────────────────┘
```

The **poller** drives everything on a timer: it asks the **detector** for each
event's state (which renders via the **browser** and classifies via **detect**),
persists results through the **postgres** store, and on a notable transition
calls the **notify** bot to DM subscribers. The bot also serves inbound commands
(`/start`, `/stop`, `/status`).

## 3. Directory Structure

- `cmd/` — entry points (`package main`):
  - `cmd/watcher` — the production binary; wires config, DB+migrations, bot,
    browser, and poller, with graceful SIGTERM shutdown. This is what runs in Docker.
  - `cmd/bot` — a standalone bot smoke test using an in-memory subscriber store.
  - `cmd/mocksite` — runs the local mock tiket.com server (see `internal/mocksite`).
  - `cmd/spike` — a throwaway diagnostic that dumps a page's rendered HTML +
    screenshot; kept for ad-hoc selector inspection.
- `internal/` — the library packages (one responsibility each):
  - `detect` — pure HTML classification (no browser dependency).
  - `browser` — the headless-Chrome reliability layer (implements `detect.Renderer`).
  - `notify` — the Telegram bot, subscriber store interface + in-memory impl, and
    the broadcast/send functions.
  - `poll` — the orchestration state machine and all its collaborator interfaces.
  - `postgres` — pgx pool, embedded goose migrations, and the concrete stores.
  - `config` — YAML config schema, loader, validation, and per-event rule merging.
  - `mocksite` — an in-memory HTTP server that serves a tiket-like event page whose
    availability can be flipped at runtime (used for end-to-end testing).
- `internal/postgres/migrations/` — versioned SQL migrations applied by goose
  (`00001_init.sql`, `00002_event_state_soldout_notified.sql`).
- `testdata/fixtures/` — saved tiket.com HTML (`sold_out_bts.html`,
  `on_sale_weeknd.html`, `on_sale_lany.html`) plus a synthetic `blocked_challenge.html`,
  used by `internal/detect` tests.
- Root files: `config.yaml` (committed config), `config.example.yaml` (annotated
  reference), `config.demo.yaml` (points at the mock site), `.env.example`,
  `Dockerfile`, `docker-compose.yml`, `README.md`, `DEMO.md`.

## 4. Core Abstractions

**`detect.Classify(html string, r Rules) Result`** (`internal/detect/detect.go`)
is the heart of detection — a pure function over rendered HTML (parsed with
`goquery`) that returns one of `StateUnknown` / `StateSoldOut` / `StateAvailable`
plus a human-readable reason. `detect.Rules` is the config-driven spec
(`PageReadySelector`, a `SoldOutRule` of `CTASelector`/`TextAny`/`RequireDisabled`,
and `ChallengeMarkers`). `detect.Detect(ctx, Renderer, url, Rules)` glues a
`Renderer` to `Classify`; any render error becomes `UNKNOWN`.

**`browser.Browser`** (`internal/browser/browser.go`) implements
`detect.Renderer` (`Render(ctx, url, pageReadySelector) (string, error)`). It owns
one reused go-rod Chrome instance, renders each check on a fresh stealth page with
a per-check timeout, recreates the browser if it dies, and retries transient
errors with backoff. `ErrPageNotReady` (the page-ready selector never appeared) is
returned without retry and maps to `UNKNOWN`.

**`poll.Poller`** (`internal/poll/poll.go`) is the state machine. It depends on
the interfaces `EventStore`, `Detector`, `Notifier`, `Clock`, and `ActiveWindow`,
plus a `Settings` struct. `poll.RendererDetector` adapts a `detect.Renderer` into
a `Detector`; `poll.SystemClock` is the production clock. `EventState` /
`RecordOutcome` / `NotifyKind` are the persisted-state data models.

**`notify.Bot`** (`internal/notify/bot.go`) wraps a telebot long-poller. Key
collaborators are the `SubscriberStore` interface (with `MemoryStore` for
tests/`cmd/bot`) and the optional `StatusProvider` interface (powers `/status`).
`Broadcast` and `SendTo` are the send-notification functions the poller calls.

**`postgres.EventStore` / `postgres.SubscriberStore`** (`internal/postgres/`)
are the production implementations of `poll.EventStore` and
`notify.SubscriberStore`. `EventStore.Record` applies an observation atomically
(updates state, bumps/resets `unknown_streak`, appends a `state_transitions` row
only on an actual change).

**`config.Config`** (`internal/config/config.go`) models `config.yaml`;
`config.Load` parses/validates it, and `Config.RulesFor(event)` overlays a
per-event detection override onto `detection_defaults`.

## 5. Data Flow

**Polling (the main loop).** `Poller.Run` seeds every config event into
`event_state` (`EnsureEvents`, `INSERT ... ON CONFLICT DO NOTHING`) then loops
`interval ± jitter`. Each tick, `Sweep` calls `check` per event:

1. `EventStore.Get` loads the stored state.
2. `Detector.Detect` → `detect.Detect` → `browser.Render` fetches rendered HTML →
   `detect.Classify` returns `AVAILABLE` / `SOLD_OUT` / `UNKNOWN`.
3. On a `SOLD_OUT → AVAILABLE` flip, **confirm-before-alert**: wait
   `confirm_delay`, detect once more; the re-check is authoritative. The
   `AVAILABLE → SOLD_OUT` direction alerts immediately (no confirm).
4. `EventStore.Record` persists the state, streak, and (on change) a transition row.
5. If the streak crosses `unknown_streak_threshold`, DM the admin a warning
   (`Notifier.SendTo`, requires `ADMIN_CHAT_ID`).
6. On a confirmed drop or a sold-out transition (each subject to its own
   cooldown), `Notifier.Broadcast` DMs all active subscribers and
   `MarkNotified(kind)` records the per-direction timestamp.

**Telegram inbound.** The bot long-polls Telegram. `/start` upserts the sender
into `subscribers` (reactivating if previously stopped); `/stop` deactivates;
`/status` calls the `StatusProvider` (the `statusReporter` in `cmd/watcher`),
which joins the config event list with `EventStore.AllStates` and replies with
each event's `last_state` and `last_checked_at`.

## 6. Dependencies & Integrations

- **tiket.com** — the watched site; loaded via headless Chrome over HTTPS.
- **Telegram Bot API** — long-polling only (no public webhook); token from env.
- **PostgreSQL** — runtime state + subscribers.
- `github.com/go-rod/rod` + `github.com/go-rod/stealth` — drive headless Chrome
  (navigation, waits, screenshots, anti-bot evasion).
- `github.com/PuerkitoBio/goquery` — parse/query rendered HTML in `detect`.
- `gopkg.in/telebot.v3` — Telegram bot framework.
- `github.com/jackc/pgx/v5` (+ `pgxpool`, `stdlib`) — Postgres driver/pool.
- `github.com/pressly/goose/v3` — embedded SQL migrations (run at startup).
- `gopkg.in/yaml.v3` — config parsing.
- `github.com/joho/godotenv` — loads `.env` in `cmd/watcher`.

## 7. Configuration

Two-way split, enforced deliberately:

- **`config.yaml`** (committed, no secrets) — poll timing (`interval`, `jitter`,
  `confirm_delay`, `cooldown_minutes`, `unknown_streak_threshold`), optional
  `active_hours` (timezone window), `browser` (timeout, user agent),
  `detection_defaults`, and the `events` list (`key`, `name`, `url`, optional
  per-event `detection` override). `config.example.yaml` is the annotated
  reference; `config.demo.yaml` targets the local mock site.
- **`.env`** (gitignored) — secrets/runtime: `TELEGRAM_BOT_TOKEN`, `DATABASE_URL`,
  optional `ADMIN_CHAT_ID`, `CHROME_BIN`, `LOG_LEVEL`, and the `POSTGRES_*` vars
  used by compose. `.env.example` is the template.

`cmd/watcher` reads `CONFIG_PATH` (default `config.yaml`) and the env vars above;
`TELEGRAM_BOT_TOKEN` and `DATABASE_URL` are required. New-developer setup:
`cp .env.example .env` and fill it in, edit `config.yaml`, then `docker compose up
-d --build` (see `README.md`). `DEMO.md` describes an end-to-end run against the
local mock site to verify a real DM.

## 8. Testing Strategy

Tests live beside the code as `_test.go` files. Run everything with `go test
./...`.

- **Unit (default, no external deps):** `internal/detect` (classification over the
  `testdata/fixtures` HTML), `internal/poll` (the state machine with fake
  store/detector/notifier and a fake clock), `internal/notify` (handlers +
  broadcast with a fake sender + in-memory store), `internal/config`.
- **Integration (gated):** `internal/postgres/*_test.go` run only when
  `TEST_DATABASE_URL` is set; otherwise they `t.Skip`. They exercise the real
  migrations, the stores, and an end-to-end poller-against-Postgres scenario.
  Example: `docker run -d -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=tikom -p
  5432:5432 postgres:16` then
  `TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/tikom?sslmode=disable' go test ./internal/postgres/`.
- **Live browser:** `internal/browser` tests launch real headless Chrome against a
  local `httptest` server (skip if no Chrome found). `live_test.go` additionally
  hits real tiket.com and runs only when `TIKOM_LIVE=1`.

## 9. Known Quirks & Gotchas

- **The "Habis" trap.** On tiket.com, "Terjual habis" (sold out) text appears in
  `<span>` category badges even on on-sale pages, so a page-wide text search would
  false-positive. The sold-out rule is therefore scoped to a `<button>`
  (`SoldOutRule.CTASelector`, default `button`) that is disabled and whose text
  matches `text_any`. Don't loosen this to a whole-page text match.
- **Challenge markers are advisory only.** Strings like `challenge-platform` show
  up on normal pages (Cloudflare telemetry), so `ChallengeMarkers` never change
  the state — they only enrich the `UNKNOWN` reason when the page-ready selector is
  absent. The real "is this page usable" signal is whether
  `page_ready_selector` (default `[data-testid="product-card"]`) rendered.
- **CSS-module class hashes are fragile.** Selectors avoid hashed classes (e.g.
  `Button_variant_primary__T0OYj`) in favor of element type + text + `disabled` +
  stable `data-testid`. If tiket.com changes its markup, detection silently drifts
  to `AVAILABLE`/`UNKNOWN`; the `unknown_streak` admin warning is the main signal
  for that.
- **Two cooldowns, two columns.** `last_notified_at` is the drop (AVAILABLE)
  cooldown; `last_soldout_notified_at` (added in migration `00002`) is the sold-out
  cooldown. They're independent so one direction never suppresses the other, but
  both use the same `cooldown_minutes` duration.
- **Edge alerts are only between known states.** Notifications fire on
  `SOLD_OUT → AVAILABLE` and `AVAILABLE → SOLD_OUT` only. If the page goes
  `SOLD_OUT → UNKNOWN → AVAILABLE` (e.g. a blocked period), no drop alert fires —
  this is intentional and mitigated by the unknown-streak warning.
- **Confirm-before-alert is asymmetric.** Only drops are re-checked; sold-out
  fires on first detection. A glitchy "sold out" read can thus produce a false
  sold-out alert (then a drop alert next cycle).
- **Event identity is `event_key`, not the URL.** Changing an event's `url`/slug in
  config keeps its history. `EnsureEvents` uses `ON CONFLICT (event_key) DO NOTHING`.
- **`cmd/spike` and `cmd/bot` are dev tools**, not part of the running service;
  only `cmd/watcher` is. Phase-N references in comments are historical build notes.
- **Toolchain vs image.** `go.mod` declares `go 1.25.7`; the `Dockerfile` builds
  with `golang:1.26-bookworm` and runs on `chromedp/headless-shell`. The watcher
  imports `_ "time/tzdata"` so `active_hours` timezones work in the minimal image.
