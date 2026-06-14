package notify

import (
	"context"
	"reflect"
	"testing"
)

func TestMemoryStore_UpsertAndActiveIDs(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()

	if err := m.UpsertSubscriber(ctx, Subscriber{ChatID: 10, Username: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := m.UpsertSubscriber(ctx, Subscriber{ChatID: 20, Username: "b"}); err != nil {
		t.Fatal(err)
	}

	ids, err := m.ActiveChatIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{10, 20}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("active = %v; want %v", ids, want)
	}
}

func TestMemoryStore_UpsertIsIdempotentAndUpdates(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()

	_ = m.UpsertSubscriber(ctx, Subscriber{ChatID: 10, Username: "old", FirstName: "X"})
	_ = m.UpsertSubscriber(ctx, Subscriber{ChatID: 10, Username: "new", FirstName: "Y"})

	ids, _ := m.ActiveChatIDs(ctx)
	if want := []int64{10}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("active = %v; want single id %v (upsert must not duplicate)", ids, want)
	}
	if got := m.rows[10].sub.Username; got != "new" {
		t.Errorf("username = %q; want updated value %q", got, "new")
	}
}

func TestMemoryStore_DeactivateAndReactivate(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	_ = m.UpsertSubscriber(ctx, Subscriber{ChatID: 10})

	if err := m.DeactivateSubscriber(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if ids, _ := m.ActiveChatIDs(ctx); len(ids) != 0 {
		t.Fatalf("active = %v; want empty after deactivate", ids)
	}

	// /start after /stop must re-activate.
	_ = m.UpsertSubscriber(ctx, Subscriber{ChatID: 10})
	if ids, _ := m.ActiveChatIDs(ctx); !reflect.DeepEqual(ids, []int64{10}) {
		t.Fatalf("active = %v; want [10] after re-subscribe", ids)
	}
}

func TestMemoryStore_DeactivateUnknownIsNoop(t *testing.T) {
	if err := NewMemoryStore().DeactivateSubscriber(context.Background(), 999); err != nil {
		t.Fatalf("deactivate unknown should be a no-op, got %v", err)
	}
}
