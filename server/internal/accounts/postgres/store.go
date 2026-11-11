package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jacko/server/internal/accounts"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Create(ctx context.Context, email string, credential accounts.Credential) (int64, error) {
	var userID int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "INSERT INTO users DEFAULT VALUES RETURNING id").Scan(&userID); err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		result, err := tx.Exec(ctx, `
			INSERT INTO accounts (user_id, email, password_salt, password_hash)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (email) DO NOTHING
		`, userID, email, credential.Salt[:], credential.Hash[:])
		if err != nil {
			return fmt.Errorf("create account: %w", err)
		}
		if result.RowsAffected() == 0 {
			return accounts.ErrEmailTaken
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return userID, nil
}

func (s *Store) Find(ctx context.Context, email string) (accounts.Credential, error) {
	var credential accounts.Credential
	var salt, hash []byte
	err := s.pool.QueryRow(ctx, `
		SELECT user_id, password_salt, password_hash FROM accounts WHERE email = $1
	`, email).Scan(&credential.UserID, &salt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return accounts.Credential{}, accounts.ErrInvalidCredentials
	}
	if err != nil {
		return accounts.Credential{}, fmt.Errorf("find credential: %w", err)
	}
	copy(credential.Salt[:], salt)
	copy(credential.Hash[:], hash)
	return credential, nil
}
