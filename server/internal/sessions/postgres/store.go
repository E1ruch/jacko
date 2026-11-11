package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jacko/server/internal/sessions"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) FindActiveUser(ctx context.Context, tokenHash [sha256.Size]byte) (int64, error) {
	var userID int64
	err := s.pool.QueryRow(ctx, `
		SELECT user_id FROM sessions
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()
	`, tokenHash[:]).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, sessions.ErrInvalid
	}
	if err != nil {
		return 0, fmt.Errorf("lookup session: %w", err)
	}
	return userID, nil
}

func (s *Store) Create(ctx context.Context, userID int64, access, refresh [sha256.Size]byte) (time.Time, time.Time, error) {
	var expires, refreshExpires time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO sessions (user_id, token_hash, refresh_hash, expires_at, refresh_expires_at)
		VALUES ($1, $2, $3, now() + interval '15 minutes', now() + interval '720 hours')
		RETURNING expires_at, refresh_expires_at
	`, userID, access[:], refresh[:]).Scan(&expires, &refreshExpires)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("create session: %w", err)
	}
	return expires, refreshExpires, nil
}

func (s *Store) Rotate(ctx context.Context, oldRefresh, access, refresh [sha256.Size]byte) (int64, time.Time, time.Time, error) {
	var userID int64
	var expires, refreshExpires time.Time
	err := s.pool.QueryRow(ctx, `
		UPDATE sessions
		SET token_hash = $2, refresh_hash = $3,
		    expires_at = least(now() + interval '15 minutes', refresh_expires_at)
		WHERE refresh_hash = $1 AND revoked_at IS NULL AND refresh_expires_at > now()
		RETURNING user_id, expires_at, refresh_expires_at
	`, oldRefresh[:], access[:], refresh[:]).Scan(&userID, &expires, &refreshExpires)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, time.Time{}, sessions.ErrInvalid
	}
	if err != nil {
		return 0, time.Time{}, time.Time{}, fmt.Errorf("rotate session: %w", err)
	}
	return userID, expires, refreshExpires, nil
}

func (s *Store) Revoke(ctx context.Context, access [sha256.Size]byte) error {
	result, err := s.pool.Exec(ctx, `
		UPDATE sessions SET revoked_at = coalesce(revoked_at, now()) WHERE token_hash = $1
	`, access[:])
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	if result.RowsAffected() == 0 {
		return sessions.ErrInvalid
	}
	return nil
}
