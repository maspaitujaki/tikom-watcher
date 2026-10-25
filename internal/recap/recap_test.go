package recap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/dimasfaid/tikom/internal/detect"
	"github.com/dimasfaid/tikom/internal/poll"
)

// --- fakes ---

type fakeStore struct {
	states  map[string]poll.EventState
	last    *time.Time
	marked  []time.Time
	readErr error
}

func (f *fakeStore) AllStates(context.Context) (map[string]poll.EventState, error) {
	return f.states, f.readErr
}
func (f *fakeStore) LastRecapAt(context.Context) (*time.Time, error) { return f.last, nil }
func (f *fakeStore) MarkRecapSent(_ context.Context, at time.Time) error {
	f.marked = append(f.marked, at)
	return nil
}

type fakeNotifier struct {
	broadcasts []string
	n          int
	err        error
}

func (n *fakeNotifier) Broadcast(_ context.Context, t string) (int, error) {
	n.broadcasts = append(n.broadcasts, t)
	return n.n, n.err
}
func (n *fakeNotifier) SendTo(context.Context, int64, string) error { return nil }

// fakeClock advances time on Sleep, except the max-th call: that one records the
// requested wait, cancels ctx and fails, so Run stops after max-1 completed sleeps.
type fakeClock struct {
	t      time.Time
	sleeps []time.Duration
	max    int
	cancel context.CancelFunc
}

func (c *fakeClock) Now() time.Time { return c.t }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.sleeps = append(c.sleeps, d)
	if len(c.sleeps) >= c.max {
		c.cancel()
		return ctx.Err()
	}
	c.t = c.t.Add(d)
	return nil
}

// --- harness ---

var jkt = mustLoc("Asia/Jakarta")

func mustLoc(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func wib(day, hour, min int) time.Time {
	return time.Date(2026, 10, day, hour, min, 0, 0, jkt)
}

var testEvents = []poll.Event{
	{Key: "a", Name: "Event A"},
	{Key: "b", Name: "Event B"},
}

func newHarness(now time.Time, maxSleeps int) (*Recapper, *fakeStore, *fakeNotifier, *fakeClock, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &fakeStore{states: map[string]poll.EventState{}}
	ntf := &fakeNotifier{n: 1}
	clk := &fakeClock{t: now, max: maxSleeps, cancel: cancel}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := New(testEvents, store, ntf, clk, jkt, 21*60, log)
	return r, store, ntf, clk, ctx
}

func ptr(t time.Time) *time.Time { return &t }

// --- scheduling ---

func TestRun_WaitsUntilSendTimeThenSendsOnce(t *testing.T) {
	r, store, ntf, clk, ctx := newHarness(wib(25, 20, 0), 2)

	if err := r.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(ntf.broadcasts) != 1 {
		t.Fatalf("broadcasts = %d; want 1", len(ntf.broadcasts))
	}
	if len(store.marked) != 1 || !store.marked[0].Equal(wib(25, 21, 0)) {
		t.Fatalf("marked = %v; want one mark at 21:00 WIB", store.marked)
	}
	if want := []time.Duration{time.Hour, 24 * time.Hour}; !equalDurations(clk.sleeps, want) {
		t.Fatalf("sleeps = %v; want %v (to 21:00, then to tomorrow's 21:00)", clk.sleeps, want)
	}
}

func TestRun_AlreadySentToday_WaitsForTomorrow(t *testing.T) {
	r, store, ntf, clk, ctx := newHarness(wib(25, 21, 30), 1)
	store.last = ptr(wib(25, 21, 0))

	_ = r.Run(ctx)

	if len(ntf.broadcasts) != 0 {
		t.Fatalf("broadcasts = %d; want 0 (already sent today)", len(ntf.broadcasts))
	}
	if want := []time.Duration{23*time.Hour + 30*time.Minute}; !equalDurations(clk.sleeps, want) {
		t.Fatalf("sleeps = %v; want %v", clk.sleeps, want)
	}
}

func TestRun_RestartAfterSendTime_CatchesUpOnce(t *testing.T) {
	r, store, ntf, clk, ctx := newHarness(wib(25, 22, 0), 1)
	store.last = ptr(wib(24, 21, 0))

	_ = r.Run(ctx)

	if len(ntf.broadcasts) != 1 {
		t.Fatalf("broadcasts = %d; want 1 catch-up", len(ntf.broadcasts))
	}
	if want := []time.Duration{23 * time.Hour}; !equalDurations(clk.sleeps, want) {
		t.Fatalf("sleeps = %v; want %v", clk.sleeps, want)
	}
}

func TestRun_FirstEverRecapAfterSendTime_SendsImmediately(t *testing.T) {
	r, _, ntf, _, ctx := newHarness(wib(25, 23, 30), 1)

	_ = r.Run(ctx)

	if len(ntf.broadcasts) != 1 {
		t.Fatalf("broadcasts = %d; want 1", len(ntf.broadcasts))
	}
}

func TestRun_MissedYesterday_NoStaleSendNextMorning(t *testing.T) {
	r, store, ntf, clk, ctx := newHarness(wib(26, 8, 0), 1)
	store.last = ptr(wib(24, 21, 0)) // yesterday's recap never went out

	_ = r.Run(ctx)

	if len(ntf.broadcasts) != 0 {
		t.Fatalf("broadcasts = %d; want 0 (yesterday's recap is stale)", len(ntf.broadcasts))
	}
	if want := []time.Duration{13 * time.Hour}; !equalDurations(clk.sleeps, want) {
		t.Fatalf("sleeps = %v; want %v", clk.sleeps, want)
	}
}

func TestRun_QuietDay_StillSends(t *testing.T) {
	r, _, ntf, _, ctx := newHarness(wib(25, 21, 0), 1) // no states, no changes

	_ = r.Run(ctx)

	if len(ntf.broadcasts) != 1 {
		t.Fatalf("broadcasts = %d; want 1", len(ntf.broadcasts))
	}
	if !strings.Contains(ntf.broadcasts[0], "Event A — UNKNOWN") {
		t.Errorf("recap should list events even with no data:\n%s", ntf.broadcasts[0])
	}
}

func TestRun_BroadcastFailsToEveryone_NotMarkedAndRetried(t *testing.T) {
	r, store, ntf, clk, ctx := newHarness(wib(25, 21, 0), 1)
	ntf.n, ntf.err = 0, errors.New("telegram down")

	_ = r.Run(ctx)

	if len(store.marked) != 0 {
		t.Fatalf("marked = %v; want none so the recap is retried", store.marked)
	}
	if want := []time.Duration{retryDelay}; !equalDurations(clk.sleeps, want) {
		t.Fatalf("sleeps = %v; want %v", clk.sleeps, want)
	}
}

func TestRun_PartialBroadcastFailure_StillMarked(t *testing.T) {
	r, store, ntf, _, ctx := newHarness(wib(25, 21, 0), 1)
	ntf.n, ntf.err = 1, errors.New("one subscriber blocked the bot")

	_ = r.Run(ctx)

	if len(store.marked) != 1 {
		t.Fatalf("marked = %d; want 1 (retrying would duplicate for those who got it)", len(store.marked))
	}
}

func TestRun_StateReadFails_RetriedWithoutBroadcast(t *testing.T) {
	r, store, ntf, clk, ctx := newHarness(wib(25, 21, 0), 1)
	store.readErr = errors.New("db down")

	_ = r.Run(ctx)

	if len(ntf.broadcasts) != 0 || len(store.marked) != 0 {
		t.Fatalf("broadcasts=%d marked=%d; want none", len(ntf.broadcasts), len(store.marked))
	}
	if want := []time.Duration{retryDelay}; !equalDurations(clk.sleeps, want) {
		t.Fatalf("sleeps = %v; want %v", clk.sleeps, want)
	}
}

// --- message ---

func TestFormatRecap(t *testing.T) {
	states := map[string]poll.EventState{
		"a": {
			LastState: detect.StateAvailable, LastChangedAt: ptr(wib(25, 15, 7)),
			ChecksSinceRecap: 3000, UnknownSinceRecap: 1,
		},
		"b": {
			LastState: detect.StateSoldOut, LastChangedAt: ptr(wib(23, 11, 20)),
			ChecksSinceRecap: 2712, UnknownSinceRecap: 2,
		},
	}

	got := formatRecap(wib(25, 21, 0), ptr(wib(24, 21, 0)), testEvents, states, jkt)

	want := "📊 Daily recap — 2026-10-25 21:00 WIB\n" +
		"\n• Event A — AVAILABLE (since 15:07 WIB)" +
		"\n• Event B — SOLD_OUT (since 2026-10-23 11:20 WIB)" +
		"\n\n🩺 Watcher health (since 2026-10-24 21:00 WIB)" +
		"\nChecks: 5712 · UNKNOWN: 3 (0.05%)"
	if got != want {
		t.Fatalf("recap mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestFormatRecap_NoHistory(t *testing.T) {
	events := []poll.Event{{Key: "k"}} // no name: falls back to key
	states := map[string]poll.EventState{"k": {LastState: detect.StateUnknown}}

	got := formatRecap(wib(25, 21, 0), nil, events, states, jkt)

	for _, want := range []string{
		"• k — UNKNOWN\n", // never changed: no "since"
		"🩺 Watcher health\n",
		"Checks: 0 · UNKNOWN: 0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("recap missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "%") || strings.Contains(got, "NaN") {
		t.Errorf("zero checks must not render a percentage:\n%s", got)
	}
}

func TestFormatRecap_RendersInConfiguredZone(t *testing.T) {
	// 14:00 UTC is 21:00 WIB.
	got := formatRecap(time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC), nil, testEvents, nil, jkt)

	if !strings.HasPrefix(got, "📊 Daily recap — 2026-10-25 21:00 WIB") {
		t.Errorf("header not in WIB:\n%s", got)
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
