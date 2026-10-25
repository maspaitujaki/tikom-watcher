package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dimasfaid/tikom/internal/detect"
	"github.com/dimasfaid/tikom/internal/poll"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EventStore is the Postgres-backed poll.EventStore.
type EventStore struct {
	pool *pgxpool.Pool
}

func NewEventStore(db *DB) *EventStore {
	return &EventStore{pool: db.Pool}
}

var _ poll.EventStore = (*EventStore)(nil)

// EnsureEvents upserts the config event list. ON CONFLICT DO NOTHING means
// adding an event "just works" while existing rows keep their history — identity
// is event_key, so a slug/URL change never resets state.
func (e *EventStore) EnsureEvents(ctx context.Context, seeds []poll.EventSeed) error {
	for _, s := range seeds {
		_, err := e.pool.Exec(ctx, `
			INSERT INTO event_state (event_key, url)
			VALUES ($1, $2)
			ON CONFLICT (event_key) DO NOTHING`,
			s.Key, s.URL)
		if err != nil {
			return fmt.Errorf("ensure event %q: %w", s.Key, err)
		}
	}
	return nil
}

const eventStateColumns = `event_key, last_state, unknown_streak,
	last_changed_at, last_notified_at, last_soldout_notified_at, last_checked_at,
	checks_since_recap, unknown_since_recap`

func scanEventState(row pgx.Row) (poll.EventState, error) {
	var (
		st        poll.EventState
		lastState string
	)
	err := row.Scan(&st.EventKey, &lastState, &st.UnknownStreak,
		&st.LastChangedAt, &st.LastNotifiedAt, &st.LastSoldOutNotifiedAt, &st.LastCheckedAt,
		&st.ChecksSinceRecap, &st.UnknownSinceRecap)
	if err != nil {
		return poll.EventState{}, err
	}
	st.LastState = detect.State(lastState)
	return st, nil
}

func (e *EventStore) Get(ctx context.Context, eventKey string) (poll.EventState, error) {
	st, err := scanEventState(e.pool.QueryRow(ctx,
		`SELECT `+eventStateColumns+` FROM event_state WHERE event_key = $1`, eventKey))
	if errors.Is(err, pgx.ErrNoRows) {
		// Not seeded yet: treat as UNKNOWN so the caller can proceed.
		return poll.EventState{EventKey: eventKey, LastState: detect.StateUnknown}, nil
	}
	if err != nil {
		return poll.EventState{}, err
	}
	return st, nil
}

// AllStates returns the persisted state of every event, keyed by event_key.
func (e *EventStore) AllStates(ctx context.Context) (map[string]poll.EventState, error) {
	rows, err := e.pool.Query(ctx, `SELECT `+eventStateColumns+` FROM event_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]poll.EventState)
	for rows.Next() {
		st, err := scanEventState(rows)
		if err != nil {
			return nil, err
		}
		out[st.EventKey] = st
	}
	return out, rows.Err()
}

// Record persists an observation atomically: it bumps unknown_streak on UNKNOWN
// (resets to 0 otherwise), and on an actual state change updates last_changed_at
// and appends a state_transitions row. Every call also bumps the since-recap
// health counters (checks, and unknowns when observed is UNKNOWN).
func (e *EventStore) Record(ctx context.Context, eventKey string, observed detect.State, at time.Time) (poll.EventState, poll.RecordOutcome, error) {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return poll.EventState{}, poll.RecordOutcome{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var (
		fromStr   string
		oldStreak int
	)
	err = tx.QueryRow(ctx,
		`SELECT last_state, unknown_streak FROM event_state WHERE event_key = $1 FOR UPDATE`,
		eventKey).Scan(&fromStr, &oldStreak)
	if errors.Is(err, pgx.ErrNoRows) {
		return poll.EventState{}, poll.RecordOutcome{},
			fmt.Errorf("record: event %q not seeded (call EnsureEvents)", eventKey)
	}
	if err != nil {
		return poll.EventState{}, poll.RecordOutcome{}, err
	}

	from := detect.State(fromStr)
	changed := from != observed
	newStreak, unknownInc := 0, 0
	if observed == detect.StateUnknown {
		newStreak = oldStreak + 1
		unknownInc = 1
	}

	if changed {
		if _, err := tx.Exec(ctx, `
			UPDATE event_state SET
				last_state = $2, unknown_streak = $3,
				last_checked_at = $4, last_changed_at = $4, updated_at = now(),
				checks_since_recap = checks_since_recap + 1,
				unknown_since_recap = unknown_since_recap + $5
			WHERE event_key = $1`, eventKey, string(observed), newStreak, at, unknownInc); err != nil {
			return poll.EventState{}, poll.RecordOutcome{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO state_transitions (event_key, from_state, to_state, observed_at)
			VALUES ($1, $2, $3, $4)`,
			eventKey, fromStr, string(observed), at); err != nil {
			return poll.EventState{}, poll.RecordOutcome{}, err
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE event_state SET
				unknown_streak = $2, last_checked_at = $3, updated_at = now(),
				checks_since_recap = checks_since_recap + 1,
				unknown_since_recap = unknown_since_recap + $4
			WHERE event_key = $1`, eventKey, newStreak, at, unknownInc); err != nil {
			return poll.EventState{}, poll.RecordOutcome{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return poll.EventState{}, poll.RecordOutcome{}, err
	}

	outcome := poll.RecordOutcome{
		Changed: changed, From: from, To: observed,
		OldStreak: oldStreak, NewStreak: newStreak,
	}
	st, err := e.Get(ctx, eventKey)
	return st, outcome, err
}

func (e *EventStore) MarkNotified(ctx context.Context, eventKey string, kind poll.NotifyKind, at time.Time) error {
	column := "last_notified_at"
	if kind == poll.NotifySoldOut {
		column = "last_soldout_notified_at"
	}
	_, err := e.pool.Exec(ctx,
		`UPDATE event_state SET `+column+` = $2, updated_at = now() WHERE event_key = $1`,
		eventKey, at)
	return err
}

// LastRecapAt returns when the last daily recap was sent, or nil if none yet.
func (e *EventStore) LastRecapAt(ctx context.Context) (*time.Time, error) {
	var at time.Time
	err := e.pool.QueryRow(ctx, `SELECT last_recap_at FROM recap_state WHERE id = 1`).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &at, nil
}

// MarkRecapSent records the recap time and zeroes the since-recap health counters
// in one transaction. Checks recorded between the recap's snapshot and this reset
// are dropped; that is acceptable for a health summary.
func (e *EventStore) MarkRecapSent(ctx context.Context, at time.Time) error {
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `
		INSERT INTO recap_state (id, last_recap_at) VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET last_recap_at = EXCLUDED.last_recap_at`, at); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE event_state SET checks_since_recap = 0, unknown_since_recap = 0`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
