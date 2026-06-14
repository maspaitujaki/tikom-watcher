// Package poll is the orchestration layer: on an interval (± jitter) it runs
// detection over the config-owned event list, persists state, and DMs
// subscribers on the SOLD_OUT -> AVAILABLE edge.
//
// It depends only on interfaces (EventStore, Detector, Notifier, Clock) so the
// state machine is unit-testable with fakes and the same Poller runs against
// real Postgres + the rod renderer (Phase 4) in production.
//
// State machine per event, each cycle:
//   - detect -> observed state
//   - if prev==SOLD_OUT && observed==AVAILABLE: CONFIRM-BEFORE-ALERT — wait
//     ConfirmDelay, detect once more; the re-check is authoritative for both
//     persistence and alerting (filters single-render glitches without delay).
//   - persist via EventStore.Record (updates last_*; appends a transition only
//     on an actual change; bumps unknown_streak on UNKNOWN, resets on known).
//   - on a confirmed edge, alert active subscribers unless within cooldown.
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
	EventKey       string
	LastState      detect.State
	UnknownStreak  int
	LastChangedAt  *time.Time
	LastNotifiedAt *time.Time
	LastCheckedAt  *time.Time
}

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
	MarkNotified(ctx context.Context, eventKey string, at time.Time) error
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
	flip := prev.LastState == detect.StateSoldOut && observed == detect.StateAvailable

	if flip {
		// CONFIRM-BEFORE-ALERT: re-check once; the re-check is authoritative.
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

	// Alert only on a confirmed SOLD_OUT -> AVAILABLE edge.
	if flip && authoritative == detect.StateAvailable {
		p.alert(ctx, e, prev.LastNotifiedAt, now)
	}
}

func (p *Poller) alert(ctx context.Context, e Event, lastNotified *time.Time, now time.Time) {
	if withinCooldown(lastNotified, now, p.set.Cooldown) {
		p.log.Info("drop confirmed but within cooldown; not re-alerting",
			"event", e.Key, "cooldown", p.set.Cooldown)
		return
	}
	sent, err := p.notifier.Broadcast(ctx, availableMessage(e))
	if err != nil {
		p.log.Error("broadcast partially/fully failed", "event", e.Key, "err", err)
	}
	if err := p.store.MarkNotified(ctx, e.Key, now); err != nil {
		p.log.Error("mark notified failed", "event", e.Key, "err", err)
	}
	p.log.Info("ALERT sent: tickets available", "event", e.Key, "subscribers", sent)
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

func availableMessage(e Event) string {
	name := e.Name
	if name == "" {
		name = e.Key
	}
	return fmt.Sprintf("🎟️ Tickets available again!\n%s\n%s", name, e.URL)
}

func warningMessage(e Event, streak int) string {
	name := e.Name
	if name == "" {
		name = e.Key
	}
	return fmt.Sprintf("⚠️ %q has read UNKNOWN %d× in a row — the site may have changed or be blocking us. Check the detection config.\n%s",
		name, streak, e.URL)
}
