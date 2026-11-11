package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

var ErrInvalid = errors.New("invalid session")

type Store interface {
	FindActiveUser(ctx context.Context, tokenHash [sha256.Size]byte) (int64, error)
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) Authenticate(ctx context.Context, token string) (int64, error) {
	if len(token) != 64 {
		return 0, ErrInvalid
	}
	var secret [32]byte
	if _, err := hex.Decode(secret[:], []byte(token)); err != nil {
		return 0, ErrInvalid
	}
	return s.store.FindActiveUser(ctx, sha256.Sum256(secret[:]))
}
