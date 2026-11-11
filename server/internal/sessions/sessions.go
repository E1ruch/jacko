package sessions

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

var ErrInvalid = errors.New("invalid session")

type Store interface {
	FindActiveUser(ctx context.Context, tokenHash [sha256.Size]byte) (int64, error)
	Create(ctx context.Context, userID int64, access, refresh [sha256.Size]byte) (time.Time, time.Time, error)
	Rotate(ctx context.Context, oldRefresh, access, refresh [sha256.Size]byte) (int64, time.Time, time.Time, error)
	Revoke(ctx context.Context, access [sha256.Size]byte) error
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

type Tokens struct {
	UserID           int64     `json:"user_id,string"`
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	ExpiresAt        time.Time `json:"expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}

func (s *Service) Authenticate(ctx context.Context, token string) (int64, error) {
	hash, err := tokenHash(token)
	if err != nil {
		return 0, err
	}
	return s.store.FindActiveUser(ctx, hash)
}

func (s *Service) Issue(ctx context.Context, userID int64) (Tokens, error) {
	access, accessHash := newToken()
	refresh, refreshHash := newToken()
	expires, refreshExpires, err := s.store.Create(ctx, userID, accessHash, refreshHash)
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{
		UserID:           userID,
		AccessToken:      access,
		RefreshToken:     refresh,
		ExpiresAt:        expires,
		RefreshExpiresAt: refreshExpires,
	}, nil
}

func (s *Service) Refresh(ctx context.Context, token string) (Tokens, error) {
	oldHash, err := tokenHash(token)
	if err != nil {
		return Tokens{}, err
	}
	access, accessHash := newToken()
	refresh, refreshHash := newToken()
	userID, expires, refreshExpires, err := s.store.Rotate(ctx, oldHash, accessHash, refreshHash)
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{
		UserID:           userID,
		AccessToken:      access,
		RefreshToken:     refresh,
		ExpiresAt:        expires,
		RefreshExpiresAt: refreshExpires,
	}, nil
}

func (s *Service) Revoke(ctx context.Context, token string) error {
	hash, err := tokenHash(token)
	if err != nil {
		return err
	}
	return s.store.Revoke(ctx, hash)
}

func newToken() (string, [sha256.Size]byte) {
	var secret [32]byte
	rand.Read(secret[:])
	return hex.EncodeToString(secret[:]), sha256.Sum256(secret[:])
}

func tokenHash(token string) ([sha256.Size]byte, error) {
	if len(token) != 64 {
		return [sha256.Size]byte{}, ErrInvalid
	}
	var secret [32]byte
	if _, err := hex.Decode(secret[:], []byte(token)); err != nil {
		return [sha256.Size]byte{}, ErrInvalid
	}
	return sha256.Sum256(secret[:]), nil
}
