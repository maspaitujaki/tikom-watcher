package notify

import (
	"context"
	"sort"
	"sync"
)

// Subscriber is a Telegram user who should receive ticket-drop alerts.
type Subscriber struct {
	ChatID    int64
	Username  string
	FirstName string
}

// SubscriberStore persists subscribers. The Postgres implementation lands in
// Phase 3 (alongside the versioned migrations); MemoryStore backs tests and
// local smoke-testing until then.
type SubscriberStore interface {
	// UpsertSubscriber inserts or updates a subscriber by ChatID and ensures
	// the row is active (so /start after /stop re-activates).
	UpsertSubscriber(ctx context.Context, s Subscriber) error
	// DeactivateSubscriber marks a subscriber inactive (e.g. /stop, or after
	// they block the bot). The row is kept for history.
	DeactivateSubscriber(ctx context.Context, chatID int64) error
	// ActiveChatIDs returns the chat IDs of all active subscribers.
	ActiveChatIDs(ctx context.Context) ([]int64, error)
}

type memRow struct {
	sub    Subscriber
	active bool
}

// MemoryStore is a thread-safe, in-memory SubscriberStore.
type MemoryStore struct {
	mu   sync.Mutex
	rows map[int64]memRow
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[int64]memRow)}
}

func (m *MemoryStore) UpsertSubscriber(_ context.Context, s Subscriber) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[s.ChatID] = memRow{sub: s, active: true}
	return nil
}

func (m *MemoryStore) DeactivateSubscriber(_ context.Context, chatID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[chatID]; ok {
		r.active = false
		m.rows[chatID] = r
	}
	return nil
}

func (m *MemoryStore) ActiveChatIDs(_ context.Context) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]int64, 0, len(m.rows))
	for id, r := range m.rows {
		if r.active {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}
