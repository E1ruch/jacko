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
		SELECT user_id, password_salt, password_hash, password_version, email_verified_at IS NOT NULL FROM accounts WHERE email = $1
	`, email).Scan(&credential.UserID, &salt, &hash, &credential.PasswordVersion, &credential.EmailVerified)
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

func (s *Store) SaveCode(ctx context.Context, email string, purpose accounts.Purpose, hash [32]byte) (bool, error) {
	result, err := s.pool.Exec(ctx, `
		WITH target AS (
			SELECT user_id FROM accounts
			WHERE email = $1 AND ($2 = 'reset_password' OR email_verified_at IS NULL)
			FOR UPDATE
		)
		INSERT INTO account_codes (user_id, purpose, token_hash, expires_at)
		SELECT user_id, $2, $3, now() + CASE WHEN $2 = 'reset_password' THEN interval '30 minutes' ELSE interval '24 hours' END
		FROM target
		ON CONFLICT (user_id, purpose) DO UPDATE
		SET token_hash = excluded.token_hash, issued_at = now(), expires_at = excluded.expires_at
		WHERE account_codes.issued_at <= now() - interval '1 minute'
	`, email, purpose, hash[:])
	if err != nil {
		return false, fmt.Errorf("save account code: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

func (s *Store) ConsumeCode(ctx context.Context, purpose accounts.Purpose, hash [32]byte, credential accounts.Credential) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var userID int64
		err := tx.QueryRow(ctx, `
			SELECT a.user_id FROM accounts AS a
			JOIN account_codes AS c ON c.user_id = a.user_id
			WHERE c.token_hash = $1 AND c.purpose = $2 AND c.expires_at > now()
			FOR UPDATE OF a
		`, hash[:], purpose).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return accounts.ErrInvalidCode
		}
		if err != nil {
			return fmt.Errorf("lock account for code: %w", err)
		}
		result, err := tx.Exec(ctx, `
			DELETE FROM account_codes WHERE user_id = $1 AND purpose = $2 AND token_hash = $3 AND expires_at > now()
		`, userID, purpose, hash[:])
		if err != nil {
			return fmt.Errorf("consume account code: %w", err)
		}
		if result.RowsAffected() != 1 {
			return accounts.ErrInvalidCode
		}
		if purpose == accounts.ResetPassword {
			_, err = tx.Exec(ctx, `
				UPDATE accounts SET password_salt = $2, password_hash = $3, password_version = password_version + 1,
				    email_verified_at = coalesce(email_verified_at, now()) WHERE user_id = $1
			`, userID, credential.Salt[:], credential.Hash[:])
			if err != nil {
				return fmt.Errorf("reset account password: %w", err)
			}
			_, err = tx.Exec(ctx, "UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL", userID)
			if err != nil {
				return fmt.Errorf("revoke account sessions: %w", err)
			}
			_, err = tx.Exec(ctx, "DELETE FROM account_codes WHERE user_id = $1", userID)
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE accounts SET email_verified_at = coalesce(email_verified_at, now()) WHERE user_id = $1", userID)
		return err
	})
}
