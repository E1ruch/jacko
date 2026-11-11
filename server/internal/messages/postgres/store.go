// Package postgres implements message persistence using PostgreSQL.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jacko/server/internal/messages"
)

type Store struct {
	pool *pgxpool.Pool
}

var _ messages.Store = (*Store)(nil)

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Create(ctx context.Context, command messages.SendCommand) (messages.Message, error) {
	var message messages.Message
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		// Lock the membership and channel until commit so removal cannot race the write.
		var allowed int
		err := tx.QueryRow(ctx, `
			SELECT 1
			FROM channels AS c
			JOIN members AS m ON m.guild_id = c.guild_id
			WHERE c.id = $1 AND m.user_id = $2
			FOR SHARE OF c, m
		`, command.ChannelID, command.AuthorID).Scan(&allowed)
		if errors.Is(err, pgx.ErrNoRows) {
			return messages.ErrChannelAccess
		}
		if err != nil {
			return fmt.Errorf("check channel membership: %w", err)
		}

		err = scanMessage(tx.QueryRow(ctx, `
			INSERT INTO messages (channel_id, author_id, nonce, content)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (author_id, nonce) DO NOTHING
			RETURNING id, channel_id, author_id, content, created_at
		`, command.ChannelID, command.AuthorID, command.Nonce, command.Content), &message)
		if errors.Is(err, pgx.ErrNoRows) {
			// A separate statement sees the winning row under READ COMMITTED.
			err = scanMessage(tx.QueryRow(ctx, `
				SELECT id, channel_id, author_id, content, created_at
				FROM messages WHERE author_id = $1 AND nonce = $2
			`, command.AuthorID, command.Nonce), &message)
			if err != nil {
				return fmt.Errorf("read existing message: %w", err)
			}
			if message.ChannelID != command.ChannelID || message.Content != command.Content {
				return messages.ErrNonceConflict
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("insert message: %w", err)
		}

		payload, err := json.Marshal(message)
		if err != nil {
			return fmt.Errorf("encode message event: %w", err)
		}
		// The per-channel row serializes sequence allocation through commit.
		var seq int64
		err = tx.QueryRow(ctx, `
			INSERT INTO channel_event_heads (channel_id, last_seq) VALUES ($1, 1)
			ON CONFLICT (channel_id) DO UPDATE SET last_seq = channel_event_heads.last_seq + 1
			RETURNING last_seq
		`, message.ChannelID).Scan(&seq)
		if err != nil {
			return fmt.Errorf("allocate channel event sequence: %w", err)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO message_outbox (message_id, channel_id, channel_seq, event_type, payload)
			VALUES ($1, $2, $3, 'MESSAGE_CREATE', $4::jsonb)
		`, message.ID, message.ChannelID, seq, payload)
		if err != nil {
			return fmt.Errorf("insert outbox event: %w", err)
		}
		return nil
	})
	if err != nil {
		return messages.Message{}, fmt.Errorf("store message: %w", err)
	}
	return message, nil
}

func scanMessage(row pgx.Row, message *messages.Message) error {
	return row.Scan(&message.ID, &message.ChannelID, &message.AuthorID, &message.Content, &message.CreatedAt)
}

func (s *Store) List(ctx context.Context, query messages.HistoryQuery) ([]messages.Message, error) {
	// An authorized empty channel produces one row with NULL message columns.
	// A missing channel or membership produces no rows.
	rows, err := s.pool.Query(ctx, `
		SELECT page.id, page.channel_id, page.author_id, page.content, page.created_at
		FROM channels AS c
		JOIN members AS m ON m.guild_id = c.guild_id AND m.user_id = $1
		LEFT JOIN LATERAL (
			SELECT id, channel_id, author_id, content, created_at
			FROM messages
			WHERE channel_id = c.id AND ($3::bigint IS NULL OR id < $3)
			ORDER BY id DESC
			LIMIT $4
		) AS page ON true
		WHERE c.id = $2
		ORDER BY page.id DESC
	`, query.UserID, query.ChannelID, query.Before, query.Limit+1)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()

	found := make([]messages.Message, 0, query.Limit+1)
	authorized := false
	for rows.Next() {
		authorized = true
		var id, channelID, authorID *int64
		var content *string
		var createdAt *time.Time
		if err := rows.Scan(&id, &channelID, &authorID, &content, &createdAt); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		if id != nil {
			found = append(found, messages.Message{
				ID: *id, ChannelID: *channelID, AuthorID: *authorID,
				Content: *content, CreatedAt: *createdAt,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read history: %w", err)
	}
	if !authorized {
		return nil, messages.ErrChannelAccess
	}
	return found, nil
}
