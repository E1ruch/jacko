package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jacko/server/internal/attachments"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Reserve(ctx context.Context, userID, channelID int64, upload attachments.Upload) error {
	result, err := s.pool.Exec(ctx, `
		INSERT INTO attachments (id, channel_id, uploader_id, media_type, size_bytes, upload_expires_at)
		SELECT $3, c.id, $1, $4, $5, $6
		FROM channels AS c
		JOIN members AS m ON m.guild_id = c.guild_id AND m.user_id = $1
		WHERE c.id = $2
	`, userID, channelID, upload.ID, upload.MediaType, upload.SizeBytes, upload.ExpiresAt)
	if err != nil {
		return fmt.Errorf("reserve attachment: %w", err)
	}
	if result.RowsAffected() == 0 {
		return attachments.ErrUnavailable
	}
	return nil
}

func (s *Store) Owned(ctx context.Context, userID int64, id string) (attachments.Upload, error) {
	return scanUpload(s.pool.QueryRow(ctx, `
		SELECT a.id, a.media_type, a.size_bytes, coalesce(a.width, 0), coalesce(a.height, 0),
		       a.upload_expires_at, coalesce(a.object_key, '')
		FROM attachments AS a
		JOIN channels AS c ON c.id = a.channel_id
		JOIN members AS m ON m.guild_id = c.guild_id AND m.user_id = $1
		WHERE a.id = $2 AND a.uploader_id = $1
		  AND (a.object_key IS NOT NULL OR a.upload_expires_at > now())
	`, userID, id))
}

func (s *Store) Finalize(ctx context.Context, userID int64, upload attachments.Upload) (bool, error) {
	result, err := s.pool.Exec(ctx, `
		UPDATE attachments AS a
		SET object_key = $3, width = $4, height = $5
		FROM channels AS c, members AS m
		WHERE a.id = $2 AND a.uploader_id = $1
		  AND c.id = a.channel_id AND m.guild_id = c.guild_id AND m.user_id = $1
		  AND a.object_key IS NULL AND a.upload_expires_at > now()
	`, userID, upload.ID, upload.ObjectKey, upload.Width, upload.Height)
	if err != nil {
		return false, fmt.Errorf("finalize attachment: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

func (s *Store) Visible(ctx context.Context, userID int64, id string) (attachments.Upload, error) {
	return scanUpload(s.pool.QueryRow(ctx, `
		SELECT a.id, a.media_type, a.size_bytes, a.width, a.height, a.upload_expires_at, a.object_key
		FROM attachments AS a
		JOIN messages AS msg ON msg.attachment_id = a.id AND msg.deleted_at IS NULL
		JOIN channels AS c ON c.id = msg.channel_id
		JOIN members AS m ON m.guild_id = c.guild_id AND m.user_id = $1
		WHERE a.id = $2 AND a.object_key IS NOT NULL
	`, userID, id))
}

func scanUpload(row pgx.Row) (attachments.Upload, error) {
	var upload attachments.Upload
	err := row.Scan(&upload.ID, &upload.MediaType, &upload.SizeBytes, &upload.Width, &upload.Height,
		&upload.ExpiresAt, &upload.ObjectKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return attachments.Upload{}, attachments.ErrUnavailable
	}
	if err != nil {
		return attachments.Upload{}, fmt.Errorf("read attachment: %w", err)
	}
	return upload, nil
}
