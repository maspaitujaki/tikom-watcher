// Command bot is a Phase-2 manual smoke test for the Telegram long-polling bot.
//
// It wires an in-memory subscriber store (so subscribers do NOT survive a
// restart — Phase 3 swaps in Postgres) and runs the bot until SIGINT/SIGTERM.
//
// Usage:
//
//	TELEGRAM_BOT_TOKEN=123:abc go run ./cmd/bot
//
// Then DM your bot /start and /stop on Telegram.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dimasfaid/tikom/internal/notify"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Error("TELEGRAM_BOT_TOKEN is not set")
		os.Exit(1)
	}

	store := notify.NewMemoryStore() // Phase 3: replace with the Postgres store
	bot, err := notify.New(notify.Config{Token: token, Store: store, Logger: log})
	if err != nil {
		log.Error("create bot failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("bot starting (long-poll); send /start in Telegram")
		bot.Start() // blocks until Stop
	}()

	<-ctx.Done()
	log.Info("signal received, stopping bot")
	bot.Stop()
}
