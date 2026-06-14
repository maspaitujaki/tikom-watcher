package postgres

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/dimasfaid/tikom/internal/detect"
	"github.com/dimasfaid/tikom/internal/notify"
	"github.com/dimasfaid/tikom/internal/poll"
)

// testDB migrates and returns a clean DB, or skips if TEST_DATABASE_URL is unset.
func testDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run Postgres integration tests")
	}
	if err := Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Pool.Exec(context.Background(),
		`TRUNCATE subscribers, event_state, state_transitions`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

func TestSubscriberStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	s := NewSubscriberStore(db)

	if err := s.UpsertSubscriber(ctx, notify.Subscriber{ChatID: 1, Username: "a", FirstName: "A"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSubscriber(ctx, notify.Subscriber{ChatID: 2, Username: "b"}); err != nil {
		t.Fatal(err)
	}
	// Upsert again with a changed username — must not duplicate, must update.
	if err := s.UpsertSubscriber(ctx, notify.Subscriber{ChatID: 1, Username: "a2"}); err != nil {
		t.Fatal(err)
	}

	ids, err := s.ActiveChatIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("active = %v; want [1 2]", ids)
	}

	if err := s.DeactivateSubscriber(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.ActiveChatIDs(ctx); len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("active after deactivate = %v; want [2]", ids)
	}

	// /start again re-activates.
	_ = s.UpsertSubscriber(ctx, notify.Subscriber{ChatID: 1})
	if ids, _ := s.ActiveChatIDs(ctx); len(ids) != 2 {
		t.Fatalf("active after re-subscribe = %v; want 2", ids)
	}
}

func TestEventStore_EnsureIdempotent_KeepsHistoryOnURLChange(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	es := NewEventStore(db)

	if err := es.EnsureEvents(ctx, []poll.EventSeed{{Key: "k", URL: "https://old"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := es.Record(ctx, "k", detect.StateSoldOut, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Re-ensure with a new URL (e.g. slug change) + an unrelated new event.
	if err := es.EnsureEvents(ctx, []poll.EventSeed{
		{Key: "k", URL: "https://new"},
		{Key: "k2", URL: "https://k2"},
	}); err != nil {
		t.Fatal(err)
	}

	st, err := es.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if st.LastState != detect.StateSoldOut {
		t.Fatalf("state = %s; want SOLD_OUT preserved across re-ensure", st.LastState)
	}
	if st2, _ := es.Get(ctx, "k2"); st2.LastState != detect.StateUnknown {
		t.Fatalf("new event default = %s; want UNKNOWN", st2.LastState)
	}
}

func TestEventStore_RecordTransitionsAndStreak(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	es := NewEventStore(db)
	now := time.Now()

	if err := es.EnsureEvents(ctx, []poll.EventSeed{{Key: "k", URL: "u"}}); err != nil {
		t.Fatal(err)
	}

	// UNKNOWN twice: streak grows, no transition (state didn't change from UNKNOWN).
	_, o1, _ := es.Record(ctx, "k", detect.StateUnknown, now)
	_, o2, _ := es.Record(ctx, "k", detect.StateUnknown, now.Add(time.Minute))
	if o1.NewStreak != 1 || o2.NewStreak != 2 {
		t.Fatalf("streaks = %d,%d; want 1,2", o1.NewStreak, o2.NewStreak)
	}
	if o1.Changed || o2.Changed {
		t.Fatal("UNKNOWN->UNKNOWN must not be a change")
	}

	// SOLD_OUT then AVAILABLE: two changes, streak resets.
	_, o3, _ := es.Record(ctx, "k", detect.StateSoldOut, now.Add(2*time.Minute))
	if !o3.Changed || o3.NewStreak != 0 {
		t.Fatalf("SOLD_OUT: changed=%v streak=%d; want true,0", o3.Changed, o3.NewStreak)
	}
	_, o4, _ := es.Record(ctx, "k", detect.StateAvailable, now.Add(3*time.Minute))
	if !o4.Changed || o4.From != detect.StateSoldOut {
		t.Fatalf("AVAILABLE: changed=%v from=%s; want true,SOLD_OUT", o4.Changed, o4.From)
	}

	var n int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM state_transitions WHERE event_key = 'k'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("transitions = %d; want 2 (UNKNOWN->SOLD_OUT, SOLD_OUT->AVAILABLE)", n)
	}

	st, _ := es.Get(ctx, "k")
	if st.LastState != detect.StateAvailable || st.LastChangedAt == nil {
		t.Fatalf("final state=%s changedAt=%v", st.LastState, st.LastChangedAt)
	}
}

// --- end-to-end: real Poller + real Postgres + fakes for detector/notifier ---

type scriptDet struct{ q []detect.Result }

func (d *scriptDet) load(rs ...detect.Result) { d.q = rs }
func (d *scriptDet) Detect(context.Context, string, detect.Rules) detect.Result {
	if len(d.q) == 0 {
		return detect.Result{State: detect.StateUnknown}
	}
	r := d.q[0]
	d.q = d.q[1:]
	return r
}

type captureNtf struct {
	broadcasts, admin int
}

func (n *captureNtf) Broadcast(context.Context, string) (int, error) { n.broadcasts++; return 1, nil }
func (n *captureNtf) SendTo(context.Context, int64, string) error    { n.admin++; return nil }

type advClock struct{ t time.Time }

func (c *advClock) Now() time.Time { return c.t }
func (c *advClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.t = c.t.Add(d)
	return nil
}

func res(s detect.State) detect.Result { return detect.Result{State: s} }

func TestPoller_EndToEnd_Postgres(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	es := NewEventStore(db)

	ev := poll.Event{Key: "e2e", Name: "E2E Show", URL: "https://x/e2e"}
	det := &scriptDet{}
	ntf := &captureNtf{}
	clk := &advClock{t: time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)}
	set := poll.Settings{
		ConfirmDelay: time.Second, Cooldown: 30 * time.Minute,
		UnknownStreakThreshold: 2, AdminChatID: 7,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := poll.New([]poll.Event{ev}, es, det, ntf, clk, set, log)

	if err := es.EnsureEvents(ctx, []poll.EventSeed{{Key: ev.Key, URL: ev.URL}}); err != nil {
		t.Fatal(err)
	}

	// Sweep 1: SOLD_OUT baseline (no alert).
	det.load(res(detect.StateSoldOut))
	p.Sweep(ctx)
	if ntf.broadcasts != 0 {
		t.Fatalf("baseline must not alert; got %d", ntf.broadcasts)
	}

	// Sweep 2: confirmed drop -> alert.
	det.load(res(detect.StateAvailable), res(detect.StateAvailable))
	p.Sweep(ctx)
	if ntf.broadcasts != 1 {
		t.Fatalf("confirmed drop must alert once; got %d", ntf.broadcasts)
	}

	st, _ := es.Get(ctx, ev.Key)
	if st.LastState != detect.StateAvailable || st.LastNotifiedAt == nil {
		t.Fatalf("DB after drop: state=%s notified=%v", st.LastState, st.LastNotifiedAt)
	}

	var n int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM state_transitions WHERE event_key = $1`, ev.Key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("transitions = %d; want 2 (UNKNOWN->SOLD_OUT, SOLD_OUT->AVAILABLE)", n)
	}
}
