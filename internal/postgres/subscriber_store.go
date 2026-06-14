package postgres

import (
	"context"

	"github.com/dimasfaid/tikom/internal/notify"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SubscriberStore is the Postgres-backed notify.SubscriberStore.
type SubscriberStore struct {
	pool *pgxpool.Pool
}

func NewSubscriberStore(db *DB) *SubscriberStore {
	return &SubscriberStore{pool: db.Pool}
}

var _ notify.SubscriberStore = (*SubscriberStore)(nil)

func (s *SubscriberStore) UpsertSubscriber(ctx context.Context, sub notify.Subscriber) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO subscribers (chat_id, username, first_name, is_active)
		VALUES ($1, $2, $3, TRUE)
		ON CONFLICT (chat_id) DO UPDATE SET
			username   = EXCLUDED.username,
			first_name = EXCLUDED.first_name,
			is_active  = TRUE`,
		sub.ChatID, sub.Username, sub.FirstName)
	return err
}

func (s *SubscriberStore) DeactivateSubscriber(ctx context.Context, chatID int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE subscribers SET is_active = FALSE WHERE chat_id = $1`, chatID)
	return err
}

func (s *SubscriberStore) ActiveChatIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT chat_id FROM subscribers WHERE is_active ORDER BY chat_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
