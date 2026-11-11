// Package accounts owns credential registration and password verification.
package accounts

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"net/mail"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

var (
	ErrInvalidInput       = errors.New("invalid account data")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrEmailTaken         = errors.New("email already registered")
	ErrBusy               = errors.New("password service busy")
)

type Credential struct {
	UserID int64
	Salt   [16]byte
	Hash   [32]byte
}

type Store interface {
	Create(ctx context.Context, email string, credential Credential) (int64, error)
	Find(ctx context.Context, email string) (Credential, error)
}

type Service struct {
	store Store
	slots chan struct{}
}

func NewService(store Store) *Service {
	return &Service{store: store, slots: make(chan struct{}, 2)}
}

func (s *Service) Register(ctx context.Context, email, password string) (int64, error) {
	email, valid := normalizeEmail(email)
	if !valid || !validPassword(password) || utf8.RuneCountInString(password) < 12 {
		return 0, ErrInvalidInput
	}

	var credential Credential
	rand.Read(credential.Salt[:])
	hash, err := s.derive(ctx, password, credential.Salt)
	if err != nil {
		return 0, err
	}
	copy(credential.Hash[:], hash)
	return s.store.Create(ctx, email, credential)
}

func (s *Service) Verify(ctx context.Context, email, password string) (int64, error) {
	email, valid := normalizeEmail(email)
	if !valid || !validPassword(password) {
		return 0, ErrInvalidCredentials
	}
	credential, err := s.store.Find(ctx, email)
	if err != nil && !errors.Is(err, ErrInvalidCredentials) {
		return 0, err
	}

	// Unknown accounts still incur the same Argon2id work as incorrect passwords.
	candidate, deriveErr := s.derive(ctx, password, credential.Salt)
	if deriveErr != nil {
		return 0, deriveErr
	}
	if subtle.ConstantTimeCompare(candidate, credential.Hash[:]) != 1 || err != nil {
		return 0, ErrInvalidCredentials
	}
	return credential.UserID, nil
}

func (s *Service) derive(ctx context.Context, password string, salt [16]byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return nil, ErrBusy
	}

	hash := argon2.IDKey([]byte(password), salt[:], 2, 64*1024, 1, 32)
	return hash, ctx.Err()
}

func normalizeEmail(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 254 {
		return "", false
	}
	for _, character := range value {
		if character < 33 || character > 126 {
			return "", false
		}
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address != value {
		return "", false
	}
	return strings.ToLower(value), true
}

func validPassword(password string) bool {
	return len(password) > 0 && len(password) <= 256 && utf8.ValidString(password)
}
