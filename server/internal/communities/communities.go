package communities

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrInvalidName = errors.New("invalid name")
	ErrUnavailable = errors.New("community or invitation unavailable")
)

const PageSize = 100

type Guild struct {
	ID      int64  `json:"id,string"`
	Name    string `json:"name"`
	OwnerID *int64 `json:"owner_id,string,omitempty"`
}

type Channel struct {
	ID      int64  `json:"id,string"`
	GuildID int64  `json:"guild_id,string"`
	Name    string `json:"name"`
}

type Member struct {
	UserID int64  `json:"user_id,string"`
	Role   string `json:"role"`
}

type CreatedGuild struct {
	Guild   Guild   `json:"guild"`
	Channel Channel `json:"channel"`
}

type Invite struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Store interface {
	CreateGuild(ctx context.Context, ownerID int64, name string) (CreatedGuild, error)
	ListGuilds(ctx context.Context, userID, after int64) ([]Guild, error)
	CreateChannel(ctx context.Context, userID, guildID int64, name string) (Channel, error)
	ListChannels(ctx context.Context, userID, guildID, after int64) ([]Channel, error)
	ListMembers(ctx context.Context, userID, guildID, after int64) ([]Member, error)
	CreateInvite(ctx context.Context, userID, guildID int64, tokenHash [32]byte) (time.Time, error)
	Join(ctx context.Context, userID int64, tokenHash [32]byte) (Guild, error)
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) CreateGuild(ctx context.Context, userID int64, name string) (CreatedGuild, error) {
	name, err := normalizeName(name)
	if err != nil {
		return CreatedGuild{}, err
	}
	return s.store.CreateGuild(ctx, userID, name)
}

func (s *Service) ListGuilds(ctx context.Context, userID, after int64) ([]Guild, error) {
	return s.store.ListGuilds(ctx, userID, after)
}

func (s *Service) CreateChannel(ctx context.Context, userID, guildID int64, name string) (Channel, error) {
	name, err := normalizeName(name)
	if err != nil {
		return Channel{}, err
	}
	return s.store.CreateChannel(ctx, userID, guildID, name)
}

func (s *Service) ListChannels(ctx context.Context, userID, guildID, after int64) ([]Channel, error) {
	return s.store.ListChannels(ctx, userID, guildID, after)
}

func (s *Service) ListMembers(ctx context.Context, userID, guildID, after int64) ([]Member, error) {
	return s.store.ListMembers(ctx, userID, guildID, after)
}

func (s *Service) CreateInvite(ctx context.Context, userID, guildID int64) (Invite, error) {
	var secret [32]byte
	rand.Read(secret[:])
	expires, err := s.store.CreateInvite(ctx, userID, guildID, sha256.Sum256(secret[:]))
	if err != nil {
		return Invite{}, err
	}
	return Invite{Token: hex.EncodeToString(secret[:]), ExpiresAt: expires}, nil
}

func (s *Service) Join(ctx context.Context, userID int64, token string) (Guild, error) {
	var secret [32]byte
	if len(token) != hex.EncodedLen(len(secret)) {
		return Guild{}, ErrUnavailable
	}
	if _, err := hex.Decode(secret[:], []byte(token)); err != nil {
		return Guild{}, ErrUnavailable
	}
	return s.store.Join(ctx, userID, sha256.Sum256(secret[:]))
}

func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 80 ||
		strings.ContainsFunc(name, unicode.IsControl) {
		return "", ErrInvalidName
	}
	return name, nil
}
