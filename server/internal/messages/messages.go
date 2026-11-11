// Package messages owns the use cases and persistence contract for text chat.
package messages

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrMessageNotFound = errors.New("message unavailable")
	ErrReplyNotFound   = errors.New("reply target unavailable")
	ErrInvalidSend     = errors.New("invalid message")
	ErrChannelAccess   = errors.New("channel is unavailable to the author")
	ErrNonceConflict   = errors.New("nonce already used for a different message")
)

type SendCommand struct {
	// AuthorID must come from the authenticated session at the transport boundary.
	AuthorID  int64
	ChannelID int64
	Nonce     string
	Content   string
	ReplyTo   *int64
}

type Message struct {
	ID        int64      `json:"id,string"`
	ChannelID int64      `json:"channel_id,string"`
	AuthorID  int64      `json:"author_id,string"`
	Content   string     `json:"content"`
	CreatedAt time.Time  `json:"created_at"`
	ReplyTo   *int64     `json:"reply_to,string,omitempty"`
	EditedAt  *time.Time `json:"edited_at,omitempty"`
}

// Store applies channel membership checks to reads and writes.
// Create saves the message and event in one transaction. List returns up to Limit+1 rows.
type Store interface {
	Create(ctx context.Context, command SendCommand) (Message, error)
	List(ctx context.Context, query HistoryQuery) ([]Message, error)
	Edit(ctx context.Context, target OwnedMessage, content string) (Message, error)
	Delete(ctx context.Context, target OwnedMessage) error
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) Send(ctx context.Context, command SendCommand) (Message, error) {
	if command.AuthorID <= 0 || command.ChannelID <= 0 ||
		!validText(command.Nonce, 64) || !validText(command.Content, 8192) ||
		(command.ReplyTo != nil && *command.ReplyTo <= 0) {
		return Message{}, ErrInvalidSend
	}

	return s.store.Create(ctx, command)
}

func validText(value string, maxBytes int) bool {
	return len(value) <= maxBytes && utf8.ValidString(value) &&
		!strings.ContainsRune(value, 0) && strings.TrimSpace(value) != ""
}

const MaxPageSize = 100

var ErrInvalidHistory = errors.New("invalid history query")

type HistoryQuery struct {
	UserID    int64
	ChannelID int64
	Before    *int64
	Limit     int
}

type HistoryPage struct {
	Messages   []Message `json:"messages"`
	NextBefore int64     `json:"next_before,string,omitempty"`
}

func (s *Service) History(ctx context.Context, query HistoryQuery) (HistoryPage, error) {
	if query.UserID <= 0 || query.ChannelID <= 0 ||
		query.Limit < 1 || query.Limit > MaxPageSize ||
		(query.Before != nil && *query.Before <= 0) {
		return HistoryPage{}, ErrInvalidHistory
	}

	found, err := s.store.List(ctx, query)
	if err != nil {
		return HistoryPage{}, err
	}
	page := HistoryPage{Messages: found}
	if len(found) > query.Limit {
		page.Messages = found[:query.Limit]
		page.NextBefore = page.Messages[query.Limit-1].ID
	}
	return page, nil
}

type OwnedMessage struct {
	UserID    int64
	ChannelID int64
	MessageID int64
}

type DeletedMessage struct {
	ID        int64     `json:"id,string"`
	ChannelID int64     `json:"channel_id,string"`
	DeletedAt time.Time `json:"deleted_at"`
}

func (s *Service) Edit(ctx context.Context, target OwnedMessage, content string) (Message, error) {
	if target.UserID <= 0 || target.ChannelID <= 0 || target.MessageID <= 0 || !validText(content, 8192) {
		return Message{}, ErrInvalidSend
	}
	return s.store.Edit(ctx, target, content)
}

func (s *Service) Delete(ctx context.Context, target OwnedMessage) error {
	if target.UserID <= 0 || target.ChannelID <= 0 || target.MessageID <= 0 {
		return ErrInvalidSend
	}
	return s.store.Delete(ctx, target)
}
