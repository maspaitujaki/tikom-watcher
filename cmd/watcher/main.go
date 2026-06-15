// Command watcher is the production entrypoint: it loads config, migrates and
// opens Postgres, starts the Telegram bot, and runs the poller (backed by the
// reused headless-browser renderer), shutting everything down gracefully on
// SIGINT/SIGTERM.
//
// Required env (from .env / the environment, never config.yaml):
//
//	TELEGRAM_BOT_TOKEN  Telegram bot token
//	DATABASE_URL        postgres://user:pass@host:5432/db?sslmode=disable
//
// Optional env:
//
//	CONFIG_PATH   path to config.yaml (default ./config.yaml)
//	ADMIN_CHAT_ID chat id for "site may have changed" warnings
//	CHROME_BIN    path to Chrome/headless-shell (default: rod-managed)
//	LOG_LEVEL     debug|info|warn|error (default info)
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata" // embed the tz database so active_hours works in a minimal image

	"github.com/dimasfaid/tikom/internal/browser"
	"github.com/dimasfaid/tikom/internal/config"
	"github.com/dimasfaid/tikom/internal/notify"
	"github.com/dimasfaid/tikom/internal/poll"
	"github.com/dimasfaid/tikom/internal/postgres"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load() // load .env if present; ignore when absent
	log := newLogger()

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfgPath := envOr("CONFIG_PATH", "config.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	dsn := os.Getenv("DATABASE_URL")
	if token == "" || dsn == "" {
		log.Error("missing required env", "need", "TELEGRAM_BOT_TOKEN and DATABASE_URL")
		return errMissingEnv
	}
	adminChatID := parseInt64(log, "ADMIN_CHAT_ID")

	// Migrate before opening the app pool.
	if err := postgres.Migrate(dsn); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := postgres.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	evStore := postgres.NewEventStore(db)

	bot, err := notify.New(notify.Config{
		Token:  token,
		Store:  postgres.NewSubscriberStore(db),
		Status: statusReporter{events: cfg.Events, store: evStore},
		Logger: log,
	})
	if err != nil {
		return err
	}

	br, err := browser.New(browser.Config{
		Timeout:   cfg.Browser.Timeout.D(),
		UserAgent: cfg.Browser.UserAgent,
		BinPath:   os.Getenv("CHROME_BIN"),
		Headless:  true,
		Logger:    log,
	})
	if err != nil {
		return err
	}
	defer func() { _ = br.Close() }()

	poller := poll.New(
		buildEvents(cfg),
		evStore,
		poll.RendererDetector(br),
		bot,
		poll.SystemClock{},
		buildSettings(cfg, adminChatID),
		log,
	)

	// Bot long-polls in the background; the poller runs until the signal cancels ctx.
	go func() {
		log.Info("telegram bot starting (long-poll)")
		bot.Start()
	}()

	log.Info("watcher running", "events", len(cfg.Events), "config", cfgPath)
	runErr := poller.Run(ctx)

	// Graceful shutdown: poller has returned; stop the bot, then defers close
	// the browser and DB.
	log.Info("shutting down")
	bot.Stop()
	return runErr
}

func buildEvents(cfg *config.Config) []poll.Event {
	events := make([]poll.Event, len(cfg.Events))
	for i, e := range cfg.Events {
		events[i] = poll.Event{
			Key:   e.Key,
			Name:  e.Name,
			URL:   e.URL,
			Rules: cfg.RulesFor(e),
		}
	}
	return events
}

// statusReporter joins the config event list (for names) with persisted state to
// power the bot's /status command.
type statusReporter struct {
	events []config.Event
	store  *postgres.EventStore
}

func (r statusReporter) EventStatuses(ctx context.Context) ([]notify.EventStatus, error) {
	states, err := r.store.AllStates(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]notify.EventStatus, 0, len(r.events))
	for _, e := range r.events {
		state := "UNKNOWN"
		var lastChecked *time.Time
		if st, ok := states[e.Key]; ok {
			if st.LastState != "" {
				state = string(st.LastState)
			}
			lastChecked = st.LastCheckedAt
		}
		out = append(out, notify.EventStatus{
			Key:           e.Key,
			Name:          e.Name,
			State:         state,
			LastCheckedAt: lastChecked,
		})
	}
	return out, nil
}

func buildSettings(cfg *config.Config, adminChatID int64) poll.Settings {
	var aw poll.ActiveWindow
	if cfg.ActiveHours != nil {
		aw = cfg.ActiveHours
	}
	return poll.Settings{
		Interval:               cfg.Poll.Interval.D(),
		Jitter:                 cfg.Poll.Jitter.D(),
		ConfirmDelay:           cfg.Poll.ConfirmDelay.D(),
		Cooldown:               cfg.Cooldown(),
		UnknownStreakThreshold: cfg.Poll.UnknownStreakThreshold,
		AdminChatID:            adminChatID,
		ActiveHours:            aw,
	}
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseInt64(log *slog.Logger, key string) int64 {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Warn("ignoring invalid env value", "key", key, "value", v)
		return 0
	}
	return n
}

var errMissingEnv = errStr("missing required environment variables")

type errStr string

func (e errStr) Error() string { return string(e) }
