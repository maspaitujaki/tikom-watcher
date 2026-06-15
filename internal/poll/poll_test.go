package poll

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/dimasfaid/tikom/internal/detect"
)

// --- fakes ---

type fakeStore struct {
	st          map[string]*EventState
	transitions [][2]detect.State
}

func newFakeStore() *fakeStore { return &fakeStore{st: map[string]*EventState{}} }

func (f *fakeStore) EnsureEvents(_ context.Context, seeds []EventSeed) error {
	for _, s := range seeds {
		if _, ok := f.st[s.Key]; !ok {
			f.st[s.Key] = &EventState{EventKey: s.Key, LastState: detect.StateUnknown}
		}
	}
	return nil
}

func (f *fakeStore) Get(_ context.Context, key string) (EventState, error) {
	if s, ok := f.st[key]; ok {
		return *s, nil
	}
	return EventState{EventKey: key, LastState: detect.StateUnknown}, nil
}

func (f *fakeStore) Record(_ context.Context, key string, observed detect.State, at time.Time) (EventState, RecordOutcome, error) {
	s, ok := f.st[key]
	if !ok {
		s = &EventState{EventKey: key, LastState: detect.StateUnknown}
		f.st[key] = s
	}
	from := s.LastState
	old := s.UnknownStreak
	changed := from != observed
	newStreak := 0
	if observed == detect.StateUnknown {
		newStreak = old + 1
	}
	s.UnknownStreak = newStreak
	s.LastState = observed
	at2 := at
	s.LastCheckedAt = &at2
	if changed {
		c := at
		s.LastChangedAt = &c
		f.transitions = append(f.transitions, [2]detect.State{from, observed})
	}
	return *s, RecordOutcome{Changed: changed, From: from, To: observed, OldStreak: old, NewStreak: newStreak}, nil
}

func (f *fakeStore) MarkNotified(_ context.Context, key string, kind NotifyKind, at time.Time) error {
	if s, ok := f.st[key]; ok {
		c := at
		if kind == NotifySoldOut {
			s.LastSoldOutNotifiedAt = &c
		} else {
			s.LastNotifiedAt = &c
		}
	}
	return nil
}

func (f *fakeStore) seed(key string, state detect.State) {
	f.st[key] = &EventState{EventKey: key, LastState: state}
}

type scriptedDetector struct{ q []detect.Result }

func (d *scriptedDetector) load(rs ...detect.Result) { d.q = rs }
func (d *scriptedDetector) Detect(_ context.Context, _ string, _ detect.Rules) detect.Result {
	if len(d.q) == 0 {
		return detect.Result{State: detect.StateUnknown, Reason: "no script"}
	}
	r := d.q[0]
	d.q = d.q[1:]
	return r
}

func res(s detect.State) detect.Result { return detect.Result{State: s} }

type fakeNotifier struct {
	broadcasts []string
	admin      []string
}

func (n *fakeNotifier) Broadcast(_ context.Context, t string) (int, error) {
	n.broadcasts = append(n.broadcasts, t)
	return 1, nil
}
func (n *fakeNotifier) SendTo(_ context.Context, _ int64, t string) error {
	n.admin = append(n.admin, t)
	return nil
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.t = c.t.Add(d)
	return nil
}

// --- harness ---

var testEvent = Event{Key: "e1", Name: "Event One", URL: "https://x/e1"}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newHarness(t *testing.T, set Settings) (*Poller, *fakeStore, *scriptedDetector, *fakeNotifier, *fakeClock) {
	t.Helper()
	store := newFakeStore()
	det := &scriptedDetector{}
	ntf := &fakeNotifier{}
	clk := &fakeClock{t: time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)}
	p := New([]Event{testEvent}, store, det, ntf, clk, set, quiet())
	return p, store, det, ntf, clk
}

func defaultSettings() Settings {
	return Settings{
		ConfirmDelay:           5 * time.Second,
		Cooldown:               30 * time.Minute,
		UnknownStreakThreshold: 3,
	}
}

// --- tests ---

func TestCheck_BaselineSoldOut_NoAlert(t *testing.T) {
	p, store, det, ntf, _ := newHarness(t, defaultSettings())
	det.load(res(detect.StateSoldOut))

	p.check(context.Background(), testEvent)

	if len(ntf.broadcasts) != 0 {
		t.Fatalf("baseline sold-out must not alert; got %d", len(ntf.broadcasts))
	}
	if store.st["e1"].LastState != detect.StateSoldOut {
		t.Fatalf("state = %s; want SOLD_OUT", store.st["e1"].LastState)
	}
}

func TestCheck_NonEdgeAvailable_NoAlert(t *testing.T) {
	// prev UNKNOWN -> AVAILABLE is not the SOLD_OUT->AVAILABLE edge.
	p, _, det, ntf, _ := newHarness(t, defaultSettings())
	det.load(res(detect.StateAvailable))

	p.check(context.Background(), testEvent)

	if len(ntf.broadcasts) != 0 {
		t.Fatalf("non-edge available must not alert; got %d", len(ntf.broadcasts))
	}
}

func TestCheck_ConfirmedDrop_Alerts(t *testing.T) {
	p, store, det, ntf, _ := newHarness(t, defaultSettings())
	store.seed("e1", detect.StateSoldOut)
	det.load(res(detect.StateAvailable), res(detect.StateAvailable)) // first + confirm

	p.check(context.Background(), testEvent)

	if len(ntf.broadcasts) != 1 {
		t.Fatalf("confirmed drop must alert once; got %d", len(ntf.broadcasts))
	}
	if !strings.Contains(ntf.broadcasts[0], testEvent.URL) {
		t.Errorf("alert should contain URL; got %q", ntf.broadcasts[0])
	}
	// The clock starts at 2026-06-14; the message must carry the timestamp.
	if !strings.Contains(ntf.broadcasts[0], "2026-06-14") {
		t.Errorf("alert should contain the observed timestamp; got %q", ntf.broadcasts[0])
	}
	if store.st["e1"].LastNotifiedAt == nil {
		t.Error("last_notified_at should be set after alert")
	}
}

func TestCheck_GlitchDrop_NoAlert_StaysSoldOut(t *testing.T) {
	p, store, det, ntf, _ := newHarness(t, defaultSettings())
	store.seed("e1", detect.StateSoldOut)
	// First read AVAILABLE (single-render glitch), confirm reads SOLD_OUT.
	det.load(res(detect.StateAvailable), res(detect.StateSoldOut))

	p.check(context.Background(), testEvent)

	if len(ntf.broadcasts) != 0 {
		t.Fatalf("glitch must not alert; got %d", len(ntf.broadcasts))
	}
	if store.st["e1"].LastState != detect.StateSoldOut {
		t.Fatalf("state = %s; want SOLD_OUT (confirm authoritative)", store.st["e1"].LastState)
	}
	if len(store.transitions) != 0 {
		t.Fatalf("no transition expected; got %v", store.transitions)
	}
}

func TestCheck_CooldownSuppressesReAlert(t *testing.T) {
	ctx := context.Background()
	p, store, det, ntf, clk := newHarness(t, defaultSettings())
	store.seed("e1", detect.StateSoldOut)

	// Count drop alerts specifically (flap steps below also emit sold-out alerts).
	dropAlerts := func() int { return countContains(ntf.broadcasts, "available again") }

	// A: confirmed drop -> drop alert #1
	det.load(res(detect.StateAvailable), res(detect.StateAvailable))
	p.check(ctx, testEvent)
	if dropAlerts() != 1 {
		t.Fatalf("want 1 drop alert; got %d", dropAlerts())
	}

	// B: flaps back to sold out
	det.load(res(detect.StateSoldOut))
	p.check(ctx, testEvent)

	// C: drops again only 10 min later -> within 30m drop cooldown -> suppressed
	clk.t = clk.t.Add(10 * time.Minute)
	det.load(res(detect.StateAvailable), res(detect.StateAvailable))
	p.check(ctx, testEvent)
	if dropAlerts() != 1 {
		t.Fatalf("re-drop within cooldown must be suppressed; got %d drop alerts", dropAlerts())
	}

	// D: well past cooldown -> drop alert #2 (go SOLD_OUT then AVAILABLE again)
	clk.t = clk.t.Add(31 * time.Minute)
	det.load(res(detect.StateSoldOut))
	p.check(ctx, testEvent)
	det.load(res(detect.StateAvailable), res(detect.StateAvailable))
	p.check(ctx, testEvent)
	if dropAlerts() != 2 {
		t.Fatalf("want 2 drop alerts after cooldown elapsed; got %d", dropAlerts())
	}
}

func countContains(msgs []string, sub string) int {
	n := 0
	for _, m := range msgs {
		if strings.Contains(strings.ToLower(m), strings.ToLower(sub)) {
			n++
		}
	}
	return n
}

func TestCheck_UnknownStreakWarnsOnCrossingOnce(t *testing.T) {
	ctx := context.Background()
	set := defaultSettings()
	set.AdminChatID = 42 // threshold is 3
	p, _, det, ntf, _ := newHarness(t, set)

	for i := 0; i < 2; i++ { // streak 1, 2 — no warn
		det.load(res(detect.StateUnknown))
		p.check(ctx, testEvent)
	}
	if len(ntf.admin) != 0 {
		t.Fatalf("no warning before threshold; got %d", len(ntf.admin))
	}

	det.load(res(detect.StateUnknown)) // streak 3 -> crossing -> warn
	p.check(ctx, testEvent)
	if len(ntf.admin) != 1 {
		t.Fatalf("want 1 warning on crossing; got %d", len(ntf.admin))
	}

	det.load(res(detect.StateUnknown)) // streak 4 -> already warned -> no repeat
	p.check(ctx, testEvent)
	if len(ntf.admin) != 1 {
		t.Fatalf("must not re-warn each poll; got %d", len(ntf.admin))
	}

	// A known state resets the streak.
	det.load(res(detect.StateSoldOut))
	p.check(ctx, testEvent)
	det.load(res(detect.StateUnknown))
	p.check(ctx, testEvent) // streak back to 1
	if len(ntf.admin) != 1 {
		t.Fatalf("streak should have reset; got %d warnings", len(ntf.admin))
	}
}

func TestCheck_UnknownStreak_NoAdmin_NoWarnNoPanic(t *testing.T) {
	ctx := context.Background()
	set := defaultSettings() // AdminChatID 0
	p, _, det, ntf, _ := newHarness(t, set)
	for i := 0; i < 4; i++ {
		det.load(res(detect.StateUnknown))
		p.check(ctx, testEvent)
	}
	if len(ntf.admin) != 0 {
		t.Fatalf("no admin configured: want 0 warnings; got %d", len(ntf.admin))
	}
}

func TestCheck_AvailableToSoldOut_AlertsImmediately(t *testing.T) {
	p, store, det, ntf, _ := newHarness(t, defaultSettings())
	store.seed("e1", detect.StateAvailable)
	det.load(res(detect.StateSoldOut)) // ONLY one result: no confirm re-check for sold-out

	p.check(context.Background(), testEvent)

	if len(ntf.broadcasts) != 1 {
		t.Fatalf("sold-out transition must alert once; got %d", len(ntf.broadcasts))
	}
	if !strings.Contains(strings.ToLower(ntf.broadcasts[0]), "sold out") {
		t.Errorf("alert should say sold out; got %q", ntf.broadcasts[0])
	}
	if !strings.Contains(ntf.broadcasts[0], "2026-06-14") {
		t.Errorf("sold-out alert should carry a timestamp; got %q", ntf.broadcasts[0])
	}
	if store.st["e1"].LastSoldOutNotifiedAt == nil {
		t.Error("last_soldout_notified_at should be set")
	}
	if store.st["e1"].LastState != detect.StateSoldOut {
		t.Fatalf("state = %s; want SOLD_OUT", store.st["e1"].LastState)
	}
	// No confirm re-check means the detector queue had exactly one item consumed.
	if len(det.q) != 0 {
		t.Errorf("sold-out path must not re-check; %d detect calls left unconsumed", len(det.q))
	}
}

func TestCheck_SoldOutCooldownIndependentOfDrop(t *testing.T) {
	ctx := context.Background()
	p, store, det, ntf, _ := newHarness(t, defaultSettings())

	// A confirmed drop sets the AVAILABLE (drop) cooldown timestamp.
	store.seed("e1", detect.StateSoldOut)
	det.load(res(detect.StateAvailable), res(detect.StateAvailable))
	p.check(ctx, testEvent)
	if len(ntf.broadcasts) != 1 {
		t.Fatalf("want 1 drop alert; got %d", len(ntf.broadcasts))
	}

	// Immediately flips to SOLD_OUT: must still alert (sold-out cooldown is separate),
	// even though we're within the drop cooldown window.
	det.load(res(detect.StateSoldOut))
	p.check(ctx, testEvent)
	if len(ntf.broadcasts) != 2 {
		t.Fatalf("sold-out alert must not be suppressed by the drop cooldown; got %d", len(ntf.broadcasts))
	}
}

func TestCheck_SoldOutCooldownSuppressesRepeat(t *testing.T) {
	ctx := context.Background()
	p, store, det, ntf, clk := newHarness(t, defaultSettings()) // cooldown 30m

	// First sold-out alert.
	store.seed("e1", detect.StateAvailable)
	det.load(res(detect.StateSoldOut))
	p.check(ctx, testEvent)
	if len(ntf.broadcasts) != 1 {
		t.Fatalf("want 1 sold-out alert; got %d", len(ntf.broadcasts))
	}

	// Flap back to AVAILABLE (a drop alert), then sold out again within cooldown.
	det.load(res(detect.StateAvailable), res(detect.StateAvailable))
	p.check(ctx, testEvent)
	clk.t = clk.t.Add(5 * time.Minute) // still < 30m sold-out cooldown
	det.load(res(detect.StateSoldOut))
	p.check(ctx, testEvent)

	soldOutAlerts := 0
	for _, m := range ntf.broadcasts {
		if strings.Contains(strings.ToLower(m), "sold out") {
			soldOutAlerts++
		}
	}
	if soldOutAlerts != 1 {
		t.Fatalf("second sold-out within cooldown must be suppressed; sold-out alerts = %d", soldOutAlerts)
	}
}

func TestRun_SeedsThenStopsOnCancel(t *testing.T) {
	set := defaultSettings()
	set.Interval = time.Minute
	p, store, _, _, _ := newHarness(t, set)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: Run should seed then return promptly

	if err := p.Run(ctx); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if _, ok := store.st["e1"]; !ok {
		t.Fatal("Run must seed events via EnsureEvents")
	}
}
