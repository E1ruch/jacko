// Package postgres implements message persistence using PostgreSQL.
package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
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
		if err := lockMembership(ctx, tx, command.AuthorID, command.ChannelID); err != nil {
			return err
		}

		if command.AttachmentID != nil {
			if err := checkAttachment(ctx, tx, command); err != nil {
				return err
			}
		}
		hash := sha256.Sum256([]byte(command.Content))
		err := scanMessage(tx.QueryRow(ctx, `
			INSERT INTO messages (channel_id, author_id, nonce, content, reply_to, original_content_hash, attachment_id)
			SELECT $1, $2, $3, $4, $5, $6, $7
            WHERE $5::bigint IS NULL OR EXISTS (
                SELECT 1 FROM messages
                WHERE channel_id = $1 AND id = $5 AND deleted_at IS NULL
                FOR SHARE
            )
			ON CONFLICT DO NOTHING
			RETURNING id, channel_id, author_id, content, created_at, reply_to, edited_at, attachment_id
		`, command.ChannelID, command.AuthorID, command.Nonce, command.Content, command.ReplyTo, hash[:], command.AttachmentID), &message)
		if errors.Is(err, pgx.ErrNoRows) {
			return readRetry(ctx, tx, command, hash, &message)
		}
		if err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
		return appendEvent(ctx, tx, message.ID, message.ChannelID, "MESSAGE_CREATE", message)

	})
	if err != nil {
		return messages.Message{}, fmt.Errorf("store message: %w", err)
	}
	return message, nil
}

// A separate READ COMMITTED statement sees the winner of a concurrent nonce insert.
func readRetry(ctx context.Context, tx pgx.Tx, command messages.SendCommand, hash [32]byte, message *messages.Message) error {
	var originalHash []byte
	var deletedAt *time.Time
	fields := append(messageFields(message), &originalHash, &deletedAt)
	err := tx.QueryRow(ctx, `
        SELECT id, channel_id, author_id, coalesce(content, ''), created_at, reply_to, edited_at, attachment_id,
               original_content_hash, deleted_at
        FROM messages WHERE author_id = $1 AND nonce = $2
    `, command.AuthorID, command.Nonce).Scan(fields...)
	if errors.Is(err, pgx.ErrNoRows) {
		if command.AttachmentID != nil {
			var bound bool
			err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM messages WHERE attachment_id = $1)", command.AttachmentID).Scan(&bound)
			if err != nil {
				return fmt.Errorf("check attachment binding: %w", err)
			}
			if bound {
				return messages.ErrAttachmentNotFound
			}
		}
		return messages.ErrReplyNotFound
	}
	if err != nil {
		return fmt.Errorf("read existing message: %w", err)
	}
	if message.ChannelID != command.ChannelID || !bytes.Equal(originalHash, hash[:]) ||
		!sameValue(message.ReplyTo, command.ReplyTo) || !sameValue(message.AttachmentID, command.AttachmentID) {
		return messages.ErrNonceConflict
	}
	if deletedAt != nil {
		return messages.ErrMessageNotFound
	}
	return nil
}

func sameValue[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func messageFields(message *messages.Message) []any {
	return []any{&message.ID, &message.ChannelID, &message.AuthorID, &message.Content,
		&message.CreatedAt, &message.ReplyTo, &message.EditedAt, &message.AttachmentID}
}

func scanMessage(row pgx.Row, message *messages.Message) error {
	return row.Scan(messageFields(message)...)
}

func (s *Store) List(ctx context.Context, query messages.HistoryQuery) ([]messages.Message, error) {
	// An authorized empty channel produces one row with NULL message columns.
	// A missing channel or membership produces no rows.
	rows, err := s.pool.Query(ctx, `
		SELECT page.id, page.channel_id, page.author_id, page.content, page.created_at, page.reply_to, page.edited_at, page.attachment_id
		FROM channels AS c
		JOIN members AS m ON m.guild_id = c.guild_id AND m.user_id = $1
		LEFT JOIN LATERAL (
			SELECT id, channel_id, author_id, content, created_at, reply_to, edited_at, attachment_id
			FROM messages
			WHERE channel_id = c.id AND deleted_at IS NULL AND ($3::bigint IS NULL OR id < $3)
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
		var id, channelID, authorID, replyTo *int64
		var content, attachmentID *string
		var createdAt, editedAt *time.Time
		if err := rows.Scan(&id, &channelID, &authorID, &content, &createdAt, &replyTo, &editedAt, &attachmentID); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		if id != nil {
			found = append(found, messages.Message{
				ID: *id, ChannelID: *channelID, AuthorID: *authorID,
				Content: *content, CreatedAt: *createdAt, ReplyTo: replyTo, EditedAt: editedAt, AttachmentID: attachmentID,
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

func checkAttachment(ctx context.Context, tx pgx.Tx, command messages.SendCommand) error {
	var allowed int
	err := tx.QueryRow(ctx, `
        SELECT 1 FROM attachments
        WHERE id = $1 AND channel_id = $2 AND uploader_id = $3 AND object_key IS NOT NULL
        FOR SHARE
    `, command.AttachmentID, command.ChannelID, command.AuthorID).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return messages.ErrAttachmentNotFound
	}
	if err != nil {
		return fmt.Errorf("check attachment: %w", err)
	}
	return nil
}
