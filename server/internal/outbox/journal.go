package outbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jacko/server/internal/messages"
	"jacko/server/internal/realtime"
)

type Journal struct {
	pool *pgxpool.Pool
}

func NewJournal(pool *pgxpool.Pool) *Journal {
	return &Journal{pool: pool}
}

// Head returns the latest committed sequence and checks the caller's access.
func (j *Journal) Head(ctx context.Context, userID, channelID int64) (int64, error) {
	var head int64
	err := j.pool.QueryRow(ctx, `
		SELECT coalesce(h.last_seq, 0)
		FROM channels AS c
		JOIN members AS m ON m.guild_id = c.guild_id AND m.user_id = $1
		LEFT JOIN channel_event_heads AS h ON h.channel_id = c.id
		WHERE c.id = $2
	`, userID, channelID).Scan(&head)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, messages.ErrChannelAccess
	}
	if err != nil {
		return 0, fmt.Errorf("read channel event head: %w", err)
	}
	return head, nil
}

// Read returns one ordered replay batch. The gateway authorizes access via Head.
func (j *Journal) Read(ctx context.Context, channelID, after, through int64) ([]realtime.Event, error) {
	rows, err := j.pool.Query(ctx, `
		SELECT id, channel_seq, event_type, payload
		FROM message_outbox
		WHERE channel_id = $1 AND channel_seq > $2 AND channel_seq <= $3
		ORDER BY channel_seq
		LIMIT 32
	`, channelID, after, through)
	if err != nil {
		return nil, fmt.Errorf("query channel events: %w", err)
	}
	events, err := pgx.CollectRows(rows, scanEvent)
	if err != nil {
		return nil, fmt.Errorf("read channel events: %w", err)
	}
	return events, nil
}

func scanEvent(row pgx.CollectableRow) (realtime.Event, error) {
	event := realtime.Event{Version: 1, Op: "dispatch"}
	err := row.Scan(&event.ID, &event.Seq, &event.Type, &event.Data)
	return event, err
}
