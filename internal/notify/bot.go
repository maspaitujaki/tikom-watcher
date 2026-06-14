// Package notify is the Telegram side of the watcher: a long-polling bot that
// manages subscribers (/start, /stop) and a send-notification function used by
// the poller (Phase 3) to DM subscribers on a SOLD_OUT -> AVAILABLE drop.
//
// No public webhook is used — only Telegram long-polling. The bot token comes
// from the caller (env in cmd/bot). Persistence is via the SubscriberStore
// interface so the Postgres implementation can be slotted in for Phase 3.
package notify

import (
	"context"
	"errors"
	"log/slog"
	"time"

	tele "gopkg.in/telebot.v3"
)

const defaultPollTimeout = 10 * time.Second

const (
	welcomeMsg = "✅ You're subscribed! I'll DM you the moment a watched sold-out event has tickets again.\n\nSend /stop anytime to unsubscribe."
	goodbyeMsg = "🛑 You're unsubscribed. Send /start to subscribe again."
	errReply   = "Sorry, something went wrong on my end. Please try again in a moment."
)

// sender is the subset of *tele.Bot used for outbound messages, so tests can
// inject a fake without touching the network.
type sender interface {
	Send(to tele.Recipient, what interface{}, opts ...interface{}) (*tele.Message, error)
}

// Bot wraps a telebot long-poller plus the subscriber store.
type Bot struct {
	tb    *tele.Bot
	snd   sender
	store SubscriberStore
	log   *slog.Logger
}

// Config configures New.
type Config struct {
	Token       string
	Store       SubscriberStore
	Logger      *slog.Logger // defaults to slog.Default()
	PollTimeout time.Duration // long-poll timeout; defaults to 10s
}

// New builds a long-polling bot. It contacts the Telegram API once (getMe) to
// validate the token.
func New(cfg Config) (*Bot, error) {
	if cfg.Token == "" {
		return nil, errors.New("notify: empty telegram bot token")
	}
	if cfg.Store == nil {
		return nil, errors.New("notify: nil subscriber store")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	pollTimeout := cfg.PollTimeout
	if pollTimeout <= 0 {
		pollTimeout = defaultPollTimeout
	}

	tb, err := tele.NewBot(tele.Settings{
		Token:  cfg.Token,
		Poller: &tele.LongPoller{Timeout: pollTimeout},
		OnError: func(err error, c tele.Context) {
			log.Error("telegram handler error", "err", err)
		},
	})
	if err != nil {
		return nil, err
	}

	b := &Bot{tb: tb, snd: tb, store: cfg.Store, log: log}
	b.routes()
	return b, nil
}

func (b *Bot) routes() {
	b.tb.Handle("/start", func(c tele.Context) error {
		sub := newSubscriber(c.Sender(), chatIDOf(c))
		if err := b.handleStart(context.Background(), sub); err != nil {
			return c.Send(errReply)
		}
		return c.Send(welcomeMsg)
	})
	b.tb.Handle("/stop", func(c tele.Context) error {
		if err := b.handleStop(context.Background(), chatIDOf(c)); err != nil {
			return c.Send(errReply)
		}
		return c.Send(goodbyeMsg)
	})
}

// handleStart upserts the subscriber; separated from the telebot glue so it can
// be unit-tested without a live Context.
func (b *Bot) handleStart(ctx context.Context, sub Subscriber) error {
	if err := b.store.UpsertSubscriber(ctx, sub); err != nil {
		b.log.Error("upsert subscriber failed", "chat_id", sub.ChatID, "err", err)
		return err
	}
	b.log.Info("subscriber upserted", "chat_id", sub.ChatID, "username", sub.Username)
	return nil
}

func (b *Bot) handleStop(ctx context.Context, chatID int64) error {
	if err := b.store.DeactivateSubscriber(ctx, chatID); err != nil {
		b.log.Error("deactivate subscriber failed", "chat_id", chatID, "err", err)
		return err
	}
	b.log.Info("subscriber deactivated", "chat_id", chatID)
	return nil
}

// Start runs the long-poller; it blocks until Stop is called.
func (b *Bot) Start() { b.tb.Start() }

// Stop stops the long-poller.
func (b *Bot) Stop() { b.tb.Stop() }

// Broadcast sends text to every active subscriber. It returns the number sent
// and a joined error of any failures. Subscribers who have made themselves
// permanently undeliverable (blocked the bot, deactivated account, etc.) are
// auto-deactivated so we stop trying them.
func (b *Bot) Broadcast(ctx context.Context, text string) (int, error) {
	ids, err := b.store.ActiveChatIDs(ctx)
	if err != nil {
		return 0, err
	}
	var (
		sent int
		errs error
	)
	for _, id := range ids {
		if e := b.send(id, text); e != nil {
			b.log.Warn("broadcast send failed", "chat_id", id, "err", e)
			if permanentlyUndeliverable(e) {
				if de := b.store.DeactivateSubscriber(ctx, id); de != nil {
					b.log.Error("auto-deactivate failed", "chat_id", id, "err", de)
				}
			}
			errs = errors.Join(errs, e)
			continue
		}
		sent++
	}
	return sent, errs
}

// SendTo sends text to a single chat (e.g. the admin "site may have changed"
// warning in Phase 3).
func (b *Bot) SendTo(_ context.Context, chatID int64, text string) error {
	return b.send(chatID, text)
}

func (b *Bot) send(chatID int64, text string) error {
	_, err := b.snd.Send(tele.ChatID(chatID), text, &tele.SendOptions{
		DisableWebPagePreview: true,
	})
	return err
}

func permanentlyUndeliverable(err error) bool {
	return errors.Is(err, tele.ErrBlockedByUser) ||
		errors.Is(err, tele.ErrUserIsDeactivated) ||
		errors.Is(err, tele.ErrChatNotFound) ||
		errors.Is(err, tele.ErrNotStartedByUser)
}

func chatIDOf(c tele.Context) int64 {
	if ch := c.Chat(); ch != nil {
		return ch.ID
	}
	if u := c.Sender(); u != nil {
		return u.ID
	}
	return 0
}

func newSubscriber(u *tele.User, chatID int64) Subscriber {
	s := Subscriber{ChatID: chatID}
	if u != nil {
		s.Username = u.Username
		s.FirstName = u.FirstName
		if s.ChatID == 0 {
			s.ChatID = u.ID
		}
	}
	return s
}
