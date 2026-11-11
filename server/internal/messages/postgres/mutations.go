package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"jacko/server/internal/messages"
)

func (s *Store) Edit(ctx context.Context, target messages.OwnedMessage, content string) (messages.Message, error) {
	var message messages.Message
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		current, deletedAt, err := lockOwnedMessage(ctx, tx, target)
		if err != nil {
			return err
		}
		if deletedAt != nil {
			return messages.ErrMessageNotFound
		}
		if current.Content == content {
			message = current
			return nil
		}
		err = scanMessage(tx.QueryRow(ctx, `
			UPDATE messages SET content = $2, edited_at = clock_timestamp() WHERE id = $1
			RETURNING id, channel_id, author_id, content, created_at, reply_to, edited_at
		`, target.MessageID, content), &message)
		if err != nil {
			return fmt.Errorf("edit message: %w", err)
		}
		return appendEvent(ctx, tx, message.ID, message.ChannelID, "MESSAGE_UPDATE", message)
	})
	if err != nil {
		return messages.Message{}, err
	}
	return message, nil
}

func (s *Store) Delete(ctx context.Context, target messages.OwnedMessage) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, deletedAt, err := lockOwnedMessage(ctx, tx, target)
		if err != nil || deletedAt != nil {
			return err
		}

		tombstone := messages.DeletedMessage{ID: target.MessageID, ChannelID: target.ChannelID}
		err = tx.QueryRow(ctx, `
			UPDATE messages SET content = NULL, deleted_at = clock_timestamp() WHERE id = $1
			RETURNING deleted_at
		`, target.MessageID).Scan(&tombstone.DeletedAt)
		if err != nil {
			return fmt.Errorf("delete message: %w", err)
		}
		payload, err := json.Marshal(tombstone)
		if err != nil {
			return fmt.Errorf("encode message tombstone: %w", err)
		}
		// Keep the old sequence numbers for replay while removing deleted text.
		_, err = tx.Exec(ctx, `
			UPDATE message_outbox SET event_type = 'MESSAGE_DELETE', payload = $2::jsonb
			WHERE message_id = $1
		`, tombstone.ID, payload)
		if err != nil {
			return fmt.Errorf("redact message journal: %w", err)
		}
		return appendEvent(ctx, tx, tombstone.ID, tombstone.ChannelID, "MESSAGE_DELETE", json.RawMessage(payload))
	})
}

func lockOwnedMessage(ctx context.Context, tx pgx.Tx, target messages.OwnedMessage) (messages.Message, *time.Time, error) {
	if err := lockMembership(ctx, tx, target.UserID, target.ChannelID); err != nil {
		return messages.Message{}, nil, err
	}

	var message messages.Message
	var deletedAt *time.Time
	fields := append(messageFields(&message), &deletedAt)
	err := tx.QueryRow(ctx, `
		SELECT id, channel_id, author_id, coalesce(content, ''), created_at, reply_to, edited_at, deleted_at
		FROM messages WHERE id = $1 AND channel_id = $2 AND author_id = $3
		FOR UPDATE
	`, target.MessageID, target.ChannelID, target.UserID).Scan(fields...)
	if errors.Is(err, pgx.ErrNoRows) {
		return messages.Message{}, nil, messages.ErrMessageNotFound
	}
	if err != nil {
		return messages.Message{}, nil, fmt.Errorf("lock message: %w", err)
	}
	return message, deletedAt, nil
}

func lockMembership(ctx context.Context, tx pgx.Tx, userID, channelID int64) error {
	var allowed int
	err := tx.QueryRow(ctx, `
		SELECT 1 FROM channels AS c
		JOIN members AS m ON m.guild_id = c.guild_id
		WHERE c.id = $1 AND m.user_id = $2
		FOR SHARE OF c, m
	`, channelID, userID).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return messages.ErrChannelAccess
	}
	if err != nil {
		return fmt.Errorf("check channel membership: %w", err)
	}
	return nil
}

func appendEvent(ctx context.Context, tx pgx.Tx, messageID, channelID int64, eventType string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode message event: %w", err)
	}
	// The per-channel row serializes sequence allocation through commit.
	var seq int64
	err = tx.QueryRow(ctx, `
		INSERT INTO channel_event_heads (channel_id, last_seq) VALUES ($1, 1)
		ON CONFLICT (channel_id) DO UPDATE SET last_seq = channel_event_heads.last_seq + 1
		RETURNING last_seq
	`, channelID).Scan(&seq)
	if err != nil {
		return fmt.Errorf("allocate channel event sequence: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO message_outbox (message_id, channel_id, channel_seq, event_type, payload)
		VALUES ($1, $2, $3, $4, $5::jsonb)
	`, messageID, channelID, seq, eventType, payload)
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}
