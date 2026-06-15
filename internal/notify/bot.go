// Package notify is the Telegram side of the watcher: a long-polling bot that
// manages subscribers (/start, /stop), reports watched events (/status), and
// provides the send-notification function the poller uses to DM subscribers on
// availability changes.
//
// No public webhook is used — only Telegram long-polling. The bot token comes
// from the caller (env in cmd/bot). Persistence is via the SubscriberStore
// interface so the Postgres implementation can be slotted in for Phase 3.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	tele "gopkg.in/telebot.v3"
)

const defaultPollTimeout = 10 * time.Second

// alertTimeFormat matches the poller's timestamp format for consistency.
const alertTimeFormat = "2006-01-02 15:04:05 MST"

const (
	welcomeMsg           = "✅ You're subscribed! I'll DM you whenever a watched event changes availability — both when tickets drop and when it sells out.\n\nSend /status to see what's being watched, or /stop to unsubscribe."
	goodbyeMsg           = "🛑 You're unsubscribed. Send /start to subscribe again."
	errReply             = "Sorry, something went wrong on my end. Please try again in a moment."
	statusUnavailableMsg = "Status is unavailable right now."
)

// EventStatus is one row of the /status report.
type EventStatus struct {
	Key           string
	Name          string
	State         string
	LastCheckedAt *time.Time
}

// StatusProvider supplies the current state of every watched event for /status.
type StatusProvider interface {
	EventStatuses(ctx context.Context) ([]EventStatus, error)
}

// sender is the subset of *tele.Bot used for outbound messages, so tests can
// inject a fake without touching the network.
type sender interface {
	Send(to tele.Recipient, what interface{}, opts ...interface{}) (*tele.Message, error)
}

// Bot wraps a telebot long-poller plus the subscriber store.
type Bot struct {
	tb     *tele.Bot
	snd    sender
	store  SubscriberStore
	status StatusProvider // optional; powers /status
	log    *slog.Logger
}

// Config configures New.
type Config struct {
	Token       string
	Store       SubscriberStore
	Status      StatusProvider // optional; if nil, /status replies "unavailable"
	Logger      *slog.Logger   // defaults to slog.Default()
	PollTimeout time.Duration  // long-poll timeout; defaults to 10s
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

	b := &Bot{tb: tb, snd: tb, store: cfg.Store, status: cfg.Status, log: log}
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
	b.tb.Handle("/status", func(c tele.Context) error {
		return c.Send(b.handleStatus(context.Background()))
	})
}

// handleStatus returns the formatted /status reply.
func (b *Bot) handleStatus(ctx context.Context) string {
	if b.status == nil {
		return statusUnavailableMsg
	}
	statuses, err := b.status.EventStatuses(ctx)
	if err != nil {
		b.log.Error("status provider failed", "err", err)
		return statusUnavailableMsg
	}
	return formatStatus(statuses)
}

func formatStatus(statuses []EventStatus) string {
	if len(statuses) == 0 {
		return "No events are being watched."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📋 Watching %d event(s):\n", len(statuses))
	for _, s := range statuses {
		name := s.Name
		if name == "" {
			name = s.Key
		}
		checked := "never checked"
		if s.LastCheckedAt != nil {
			checked = "checked " + s.LastCheckedAt.Format(alertTimeFormat)
		}
		fmt.Fprintf(&b, "\n• %s — %s (%s)", name, s.State, checked)
	}
	return b.String()
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
