// Package recap sends the daily recap DM: the current status of every watched
// event plus watcher health (checks and UNKNOWN results) since the previous recap.
//
// The send time is a wall-clock time in a configured timezone. The last send is
// persisted, so a restart neither skips nor repeats the day's recap: if the
// process was down at the send time it catches up once, but only until local
// midnight, so a stale recap is never delivered the next morning.
//
// The first recap after a fresh deploy sends immediately when the deploy happens
// after the send time, covering whatever was counted since the migration.
package recap

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/dimasfaid/tikom/internal/poll"
)

// retryDelay spaces attempts after a failed send or store error.
const retryDelay = time.Minute

const timeFormat = "15:04 MST"
const dateTimeFormat = "2006-01-02 15:04 MST"

// Store is the persistence the recap needs. *postgres.EventStore satisfies it.
type Store interface {
	AllStates(ctx context.Context) (map[string]poll.EventState, error)
	LastRecapAt(ctx context.Context) (*time.Time, error)
	MarkRecapSent(ctx context.Context, at time.Time) error
}

// Recapper sends the recap once per local day.
type Recapper struct {
	events   []poll.Event
	store    Store
	notifier poll.Notifier
	clock    poll.Clock
	loc      *time.Location
	atMin    int // minutes after local midnight
	log      *slog.Logger
}

// New builds a Recapper. atMin is the send time in minutes after midnight in loc.
// log defaults to slog.Default().
func New(events []poll.Event, store Store, notifier poll.Notifier, clock poll.Clock, loc *time.Location, atMin int, log *slog.Logger) *Recapper {
	if log == nil {
		log = slog.Default()
	}
	return &Recapper{
		events:   events,
		store:    store,
		notifier: notifier,
		clock:    clock,
		loc:      loc,
		atMin:    atMin,
		log:      log,
	}
}

// Run sends the recap each day until ctx is cancelled.
func (r *Recapper) Run(ctx context.Context) error {
	for {
		if err := r.clock.Sleep(ctx, r.step(ctx)); err != nil {
			return nil
		}
	}
}

// step sends the recap if it is due and returns how long to wait before the next
// look.
func (r *Recapper) step(ctx context.Context) time.Duration {
	now := r.clock.Now()
	fire := r.fireTime(now)

	last, err := r.store.LastRecapAt(ctx)
	if err != nil {
		r.log.Error("recap: read last recap time", "err", err)
		return retryDelay
	}

	if r.due(now, fire, last) {
		if !r.send(ctx, now, last) {
			return retryDelay
		}
		now = r.clock.Now()
		fire = r.fireTime(now)
	}

	if now.Before(fire) {
		return fire.Sub(now)
	}
	return r.nextDay(fire).Sub(now)
}

// fireTime is today's send time (in loc) for the day containing now.
func (r *Recapper) fireTime(now time.Time) time.Time {
	y, m, d := now.In(r.loc).Date()
	return time.Date(y, m, d, r.atMin/60, r.atMin%60, 0, 0, r.loc)
}

func (r *Recapper) nextDay(t time.Time) time.Time {
	y, m, d := t.In(r.loc).Date()
	return time.Date(y, m, d+1, r.atMin/60, r.atMin%60, 0, 0, r.loc)
}

// due reports whether today's recap is past its send time and still unsent. fire
// is derived from now's own local day, so a recap missed yesterday is never
// delivered late: after midnight "today" has moved on and the send time is ahead.
func (r *Recapper) due(now, fire time.Time, last *time.Time) bool {
	if now.Before(fire) {
		return false
	}
	return last == nil || last.Before(fire)
}

// send broadcasts the recap and records it. It reports whether the recap should be
// treated as delivered: a partial broadcast failure still counts, as retrying
// would duplicate the DM for subscribers who already got it.
func (r *Recapper) send(ctx context.Context, now time.Time, last *time.Time) bool {
	states, err := r.store.AllStates(ctx)
	if err != nil {
		r.log.Error("recap: read event states", "err", err)
		return false
	}

	n, err := r.notifier.Broadcast(ctx, formatRecap(now, last, r.events, states, r.loc))
	if err != nil {
		r.log.Error("recap: broadcast", "sent", n, "err", err)
	}
	if err != nil && n == 0 {
		return false
	}

	if err := r.store.MarkRecapSent(ctx, now); err != nil {
		r.log.Error("recap: mark sent", "err", err)
		return false
	}
	r.log.Info("recap sent", "recipients", n)
	return true
}

// formatRecap renders the recap text. since is the previous recap time (nil if
// there was none); it labels the health window.
func formatRecap(now time.Time, since *time.Time, events []poll.Event, states map[string]poll.EventState, loc *time.Location) string {
	local := now.In(loc)

	var b strings.Builder
	fmt.Fprintf(&b, "📊 Daily recap — %s\n", local.Format(dateTimeFormat))

	var checks, unknowns int
	for _, e := range events {
		name := e.Name
		if name == "" {
			name = e.Key
		}
		st, ok := states[e.Key]
		state := string(st.LastState)
		if !ok || state == "" {
			state = "UNKNOWN"
		}
		fmt.Fprintf(&b, "\n• %s — %s", name, state)
		if st.LastChangedAt != nil {
			fmt.Fprintf(&b, " (since %s)", formatSince(*st.LastChangedAt, local))
		}
		checks += st.ChecksSinceRecap
		unknowns += st.UnknownSinceRecap
	}

	b.WriteString("\n\n🩺 Watcher health")
	if since != nil {
		fmt.Fprintf(&b, " (since %s)", since.In(loc).Format(dateTimeFormat))
	}
	fmt.Fprintf(&b, "\nChecks: %d · UNKNOWN: %d", checks, unknowns)
	if checks > 0 {
		fmt.Fprintf(&b, " (%.2f%%)", float64(unknowns)/float64(checks)*100)
	}
	return b.String()
}

// formatSince shows time-only for a change on the same local day as now, else the
// date too.
func formatSince(t, now time.Time) string {
	lt := t.In(now.Location())
	ty, tm, td := lt.Date()
	ny, nm, nd := now.Date()
	if ty == ny && tm == nm && td == nd {
		return lt.Format(timeFormat)
	}
	return lt.Format(dateTimeFormat)
}
