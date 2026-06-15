// Package poll is the orchestration layer: on an interval (± jitter) it runs
// detection over the config-owned event list, persists state, and DMs
// subscribers on availability changes (both SOLD_OUT->AVAILABLE and the reverse).
//
// It depends only on interfaces (EventStore, Detector, Notifier, Clock) so the
// state machine is unit-testable with fakes and the same Poller runs against
// real Postgres + the rod renderer in production.
//
// State machine per event, each cycle:
//   - detect -> observed state
//   - SOLD_OUT -> AVAILABLE (a drop): CONFIRM-BEFORE-ALERT — wait ConfirmDelay,
//     detect once more; the re-check is authoritative for both persistence and
//     alerting (filters single-render glitches without delay).
//   - AVAILABLE -> SOLD_OUT: alert immediately (no confirm).
//   - persist via EventStore.Record (updates last_*; appends a transition only
//     on an actual change; bumps unknown_streak on UNKNOWN, resets on known).
//   - alert active subscribers unless within that direction's cooldown
//     (drop and sold-out cooldowns are tracked independently).
//   - when unknown_streak crosses the threshold, DM the admin a warning.
package poll

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/dimasfaid/tikom/internal/detect"
)

// Event is one watched page with its resolved detection rules.
type Event struct {
	Key   string
	Name  string
	URL   string
	Rules detect.Rules
}

// EventSeed is the subset persisted on startup (INSERT ... ON CONFLICT DO NOTHING).
type EventSeed struct {
	Key string
	URL string
}

// EventState mirrors the persisted event_state row the poller needs.
type EventState struct {
	EventKey              string
	LastState             detect.State
	UnknownStreak         int
	LastChangedAt         *time.Time
	LastNotifiedAt        *time.Time // AVAILABLE (drop) alert cooldown timestamp
	LastSoldOutNotifiedAt *time.Time // SOLD_OUT alert cooldown timestamp
	LastCheckedAt         *time.Time
}

// NotifyKind identifies which transition direction an alert is for; each has its
// own independent cooldown timestamp.
type NotifyKind int

const (
	NotifyAvailable NotifyKind = iota // SOLD_OUT -> AVAILABLE (a drop)
	NotifySoldOut                     // AVAILABLE -> SOLD_OUT
)

// RecordOutcome reports what changed when an observation was persisted.
type RecordOutcome struct {
	Changed   bool
	From      detect.State
	To        detect.State
	OldStreak int
	NewStreak int
}

// EventStore persists event state + transitions. Implemented by the Postgres
// store (and an in-memory fake in tests).
type EventStore interface {
	EnsureEvents(ctx context.Context, seeds []EventSeed) error
	Get(ctx context.Context, eventKey string) (EventState, error)
	Record(ctx context.Context, eventKey string, observed detect.State, at time.Time) (EventState, RecordOutcome, error)
	MarkNotified(ctx context.Context, eventKey string, kind NotifyKind, at time.Time) error
}

// Detector turns a URL + rules into a detection result.
type Detector interface {
	Detect(ctx context.Context, url string, rules detect.Rules) detect.Result
}

// RendererDetector adapts a detect.Renderer (e.g. the browser layer) into a
// Detector by running the full render + classify pipeline.
func RendererDetector(r detect.Renderer) Detector { return rendererDetector{r} }

type rendererDetector struct{ r detect.Renderer }

func (d rendererDetector) Detect(ctx context.Context, url string, rules detect.Rules) detect.Result {
	return detect.Detect(ctx, d.r, url, rules)
}

// Notifier sends messages. *notify.Bot satisfies this.
type Notifier interface {
	Broadcast(ctx context.Context, text string) (int, error)
	SendTo(ctx context.Context, chatID int64, text string) error
}

// Clock abstracts time so cooldown/confirm-delay are deterministic in tests.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the production Clock; Sleep is cancellable via ctx.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ActiveWindow optionally gates polling to certain hours.
type ActiveWindow interface {
	Active(t time.Time) bool
}

// Settings are the poller's tunables.
type Settings struct {
	Interval               time.Duration
	Jitter                 time.Duration
	ConfirmDelay           time.Duration
	Cooldown               time.Duration
	UnknownStreakThreshold int
	AdminChatID            int64        // 0 disables admin warnings
	ActiveHours            ActiveWindow // nil => always active
}

// Poller runs the loop.
type Poller struct {
	events   []Event
	store    EventStore
	detector Detector
	notifier Notifier
	clock    Clock
	log      *slog.Logger
	set      Settings
	rnd      *rand.Rand
}

// New builds a Poller. log defaults to slog.Default().
func New(events []Event, store EventStore, detector Detector, notifier Notifier, clock Clock, set Settings, log *slog.Logger) *Poller {
	if log == nil {
		log = slog.Default()
	}
	return &Poller{
		events:   events,
		store:    store,
		detector: detector,
		notifier: notifier,
		clock:    clock,
		log:      log,
		set:      set,
		rnd:      rand.New(rand.NewSource(clock.Now().UnixNano())),
	}
}

// Run seeds events then loops until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) error {
	seeds := make([]EventSeed, len(p.events))
	for i, e := range p.events {
		seeds[i] = EventSeed{Key: e.Key, URL: e.URL}
	}
	if err := p.store.EnsureEvents(ctx, seeds); err != nil {
		return fmt.Errorf("seed events: %w", err)
	}
	p.log.Info("poller started", "events", len(p.events), "interval", p.set.Interval)

	for {
		if p.set.ActiveHours == nil || p.set.ActiveHours.Active(p.clock.Now()) {
			p.Sweep(ctx)
		} else {
			p.log.Debug("outside active hours; skipping sweep")
		}
		if err := p.clock.Sleep(ctx, p.nextDelay()); err != nil {
			p.log.Info("poller stopping", "reason", err)
			return nil
		}
	}
}

// Sweep runs one detection pass over every event. Exported so callers can do a
// single run (and for integration tests); Run calls it on each tick.
func (p *Poller) Sweep(ctx context.Context) {
	for _, e := range p.events {
		if ctx.Err() != nil {
			return
		}
		p.check(ctx, e)
	}
}

// check runs one event through the state machine.
func (p *Poller) check(ctx context.Context, e Event) {
	prev, err := p.store.Get(ctx, e.Key)
	if err != nil {
		p.log.Error("get event state failed", "event", e.Key, "err", err)
		return
	}

	res := p.detector.Detect(ctx, e.URL, e.Rules)
	observed := res.State
	authoritative := observed
	isDrop := prev.LastState == detect.StateSoldOut && observed == detect.StateAvailable
	isSoldOut := prev.LastState == detect.StateAvailable && observed == detect.StateSoldOut

	if isDrop {
		// CONFIRM-BEFORE-ALERT (drops only): re-check once; the re-check is
		// authoritative. The sold-out direction alerts immediately (no confirm).
		if err := p.clock.Sleep(ctx, p.set.ConfirmDelay); err != nil {
			return // ctx cancelled
		}
		confirm := p.detector.Detect(ctx, e.URL, e.Rules)
		authoritative = confirm.State
		p.log.Info("confirm-before-alert", "event", e.Key,
			"first", observed, "confirm", authoritative)
	}

	now := p.clock.Now()
	_, outcome, err := p.store.Record(ctx, e.Key, authoritative, now)
	if err != nil {
		p.log.Error("record state failed", "event", e.Key, "err", err)
		return
	}
	p.log.Info("checked", "event", e.Key, "state", authoritative,
		"changed", outcome.Changed, "unknown_streak", outcome.NewStreak, "reason", res.Reason)

	// Admin warning when the streak crosses the threshold (once, on crossing).
	if authoritative == detect.StateUnknown && p.set.UnknownStreakThreshold > 0 &&
		outcome.OldStreak < p.set.UnknownStreakThreshold &&
		outcome.NewStreak >= p.set.UnknownStreakThreshold {
		p.warnAdmin(ctx, e, outcome.NewStreak)
	}

	switch {
	case isDrop && authoritative == detect.StateAvailable:
		// Confirmed SOLD_OUT -> AVAILABLE.
		p.alert(ctx, e, NotifyAvailable, prev.LastNotifiedAt, now)
	case isSoldOut:
		// AVAILABLE -> SOLD_OUT (immediate; authoritative == observed == SOLD_OUT).
		p.alert(ctx, e, NotifySoldOut, prev.LastSoldOutNotifiedAt, now)
	}
}

func (p *Poller) alert(ctx context.Context, e Event, kind NotifyKind, lastNotified *time.Time, now time.Time) {
	if withinCooldown(lastNotified, now, p.set.Cooldown) {
		p.log.Info("transition confirmed but within cooldown; not re-alerting",
			"event", e.Key, "kind", kind, "cooldown", p.set.Cooldown)
		return
	}
	msg := availableMessage(e, now)
	logMsg := "ALERT sent: tickets available"
	if kind == NotifySoldOut {
		msg = soldOutMessage(e, now)
		logMsg = "ALERT sent: now sold out"
	}
	sent, err := p.notifier.Broadcast(ctx, msg)
	if err != nil {
		p.log.Error("broadcast partially/fully failed", "event", e.Key, "err", err)
	}
	if err := p.store.MarkNotified(ctx, e.Key, kind, now); err != nil {
		p.log.Error("mark notified failed", "event", e.Key, "err", err)
	}
	p.log.Info(logMsg, "event", e.Key, "subscribers", sent)
}

func (p *Poller) warnAdmin(ctx context.Context, e Event, streak int) {
	if p.set.AdminChatID == 0 {
		p.log.Warn("unknown_streak threshold crossed but no admin chat configured",
			"event", e.Key, "streak", streak)
		return
	}
	if err := p.notifier.SendTo(ctx, p.set.AdminChatID, warningMessage(e, streak)); err != nil {
		p.log.Error("admin warning failed", "event", e.Key, "err", err)
	}
}

func (p *Poller) nextDelay() time.Duration {
	d := p.set.Interval
	if p.set.Jitter > 0 {
		d += time.Duration(p.rnd.Int63n(int64(p.set.Jitter) + 1))
	}
	if d <= 0 {
		d = time.Second
	}
	return d
}

func withinCooldown(last *time.Time, now time.Time, cooldown time.Duration) bool {
	if last == nil || cooldown <= 0 {
		return false
	}
	return now.Sub(*last) < cooldown
}

const alertTimeFormat = "2006-01-02 15:04:05 MST"

func availableMessage(e Event, at time.Time) string {
	return fmt.Sprintf("🎟️ Tickets available again!\n%s\n%s\n🕒 %s",
		eventName(e), e.URL, at.Format(alertTimeFormat))
}

func soldOutMessage(e Event, at time.Time) string {
	return fmt.Sprintf("🔴 Now sold out.\n%s\n%s\n🕒 %s",
		eventName(e), e.URL, at.Format(alertTimeFormat))
}

func eventName(e Event) string {
	if e.Name != "" {
		return e.Name
	}
	return e.Key
}

func warningMessage(e Event, streak int) string {
	return fmt.Sprintf("⚠️ %q has read UNKNOWN %d× in a row — the site may have changed or be blocking us. Check the detection config.\n%s",
		eventName(e), streak, e.URL)
}
