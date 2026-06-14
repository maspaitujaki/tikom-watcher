package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"

	tele "gopkg.in/telebot.v3"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeSender records successful sends and can be told to fail specific chats.
type fakeSender struct {
	mu   sync.Mutex
	sent []int64
	fail map[int64]error
}

func (f *fakeSender) Send(to tele.Recipient, _ interface{}, _ ...interface{}) (*tele.Message, error) {
	id, _ := strconv.ParseInt(to.Recipient(), 10, 64)
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.fail[id]; e != nil {
		return nil, e
	}
	f.sent = append(f.sent, id)
	return &tele.Message{}, nil
}

type failingStore struct{}

func (failingStore) UpsertSubscriber(context.Context, Subscriber) error { return errors.New("boom") }
func (failingStore) DeactivateSubscriber(context.Context, int64) error  { return errors.New("boom") }
func (failingStore) ActiveChatIDs(context.Context) ([]int64, error)     { return nil, errors.New("boom") }

func newTestBot(store SubscriberStore, snd sender) *Bot {
	return &Bot{snd: snd, store: store, log: quietLogger()}
}

func TestHandleStart_UpsertsSubscriber(t *testing.T) {
	mem := NewMemoryStore()
	b := newTestBot(mem, &fakeSender{})

	err := b.handleStart(context.Background(), Subscriber{ChatID: 42, Username: "neo"})
	if err != nil {
		t.Fatalf("handleStart: %v", err)
	}
	if ids, _ := mem.ActiveChatIDs(context.Background()); len(ids) != 1 || ids[0] != 42 {
		t.Fatalf("active = %v; want [42]", ids)
	}
}

func TestHandleStart_StoreErrorPropagates(t *testing.T) {
	b := newTestBot(failingStore{}, &fakeSender{})
	if err := b.handleStart(context.Background(), Subscriber{ChatID: 1}); err == nil {
		t.Fatal("want error when store fails")
	}
}

func TestHandleStop_Deactivates(t *testing.T) {
	mem := NewMemoryStore()
	_ = mem.UpsertSubscriber(context.Background(), Subscriber{ChatID: 7})
	b := newTestBot(mem, &fakeSender{})

	if err := b.handleStop(context.Background(), 7); err != nil {
		t.Fatalf("handleStop: %v", err)
	}
	if ids, _ := mem.ActiveChatIDs(context.Background()); len(ids) != 0 {
		t.Fatalf("active = %v; want empty after /stop", ids)
	}
}

func TestBroadcast_SendsToAllActive(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryStore()
	_ = mem.UpsertSubscriber(ctx, Subscriber{ChatID: 1})
	_ = mem.UpsertSubscriber(ctx, Subscriber{ChatID: 2})
	_ = mem.UpsertSubscriber(ctx, Subscriber{ChatID: 3})
	fs := &fakeSender{}
	b := newTestBot(mem, fs)

	sent, err := b.Broadcast(ctx, "tickets!")
	if err != nil {
		t.Fatalf("broadcast err: %v", err)
	}
	if sent != 3 || len(fs.sent) != 3 {
		t.Fatalf("sent = %d (recorded %d); want 3", sent, len(fs.sent))
	}
}

func TestBroadcast_BlockedUserAutoDeactivated(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryStore()
	_ = mem.UpsertSubscriber(ctx, Subscriber{ChatID: 1})
	_ = mem.UpsertSubscriber(ctx, Subscriber{ChatID: 2}) // will report blocked
	fs := &fakeSender{fail: map[int64]error{2: tele.ErrBlockedByUser}}
	b := newTestBot(mem, fs)

	sent, err := b.Broadcast(ctx, "tickets!")
	if sent != 1 {
		t.Fatalf("sent = %d; want 1 (chat 2 blocked)", sent)
	}
	if err == nil {
		t.Fatal("want joined error reflecting the blocked send")
	}
	if ids, _ := mem.ActiveChatIDs(ctx); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("active = %v; want [1] (chat 2 auto-deactivated)", ids)
	}
}

func TestBroadcast_TransientErrorDoesNotDeactivate(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryStore()
	_ = mem.UpsertSubscriber(ctx, Subscriber{ChatID: 1})
	fs := &fakeSender{fail: map[int64]error{1: errors.New("network blip")}}
	b := newTestBot(mem, fs)

	if sent, err := b.Broadcast(ctx, "x"); sent != 0 || err == nil {
		t.Fatalf("sent=%d err=%v; want 0 and an error", sent, err)
	}
	// Transient failure must NOT deactivate the subscriber.
	if ids, _ := mem.ActiveChatIDs(ctx); len(ids) != 1 {
		t.Fatalf("active = %v; want [1] (transient error keeps subscriber)", ids)
	}
}

func TestNewSubscriber_FallsBackToUserID(t *testing.T) {
	got := newSubscriber(&tele.User{ID: 99, Username: "u", FirstName: "F"}, 0)
	want := Subscriber{ChatID: 99, Username: "u", FirstName: "F"}
	if got != want {
		t.Fatalf("newSubscriber = %+v; want %+v", got, want)
	}
}
