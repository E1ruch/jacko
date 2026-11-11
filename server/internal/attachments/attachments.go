// Package attachments handles channel-scoped image uploads and downloads.
package attachments

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"time"
)

const MaxBytes = 8 << 20

const (
	uploadTTL   = 10 * time.Minute
	downloadTTL = time.Minute
)

var (
	ErrUnavailable = errors.New("attachment unavailable")
	ErrInvalid     = errors.New("invalid image attachment")
	ErrIncomplete  = errors.New("image upload is missing or invalid")
	ErrStorage     = errors.New("attachment storage unavailable")
	ErrBusy        = errors.New("image validation busy")
)

type Image struct {
	ID        string `json:"id"`
	MediaType string `json:"media_type"`
	SizeBytes int64  `json:"size_bytes"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

type Upload struct {
	Image
	ExpiresAt time.Time
	ObjectKey string
}

type UploadForm struct {
	ID        string            `json:"id"`
	URL       string            `json:"url"`
	Fields    map[string]string `json:"fields"`
	ExpiresAt time.Time         `json:"expires_at"`
}

type Download struct {
	Image
	URL string `json:"url"`
}

type Store interface {
	Reserve(ctx context.Context, userID, channelID int64, upload Upload) error
	Owned(ctx context.Context, userID int64, id string) (Upload, error)
	Finalize(ctx context.Context, userID int64, upload Upload) (bool, error)
	Visible(ctx context.Context, userID int64, id string) (Upload, error)
}

type Objects interface {
	SignUpload(ctx context.Context, key, mediaType string, size int64, expiresAt time.Time) (string, map[string]string, error)
	ReadUpload(ctx context.Context, key string, expectedSize int64) ([]byte, error)
	Save(ctx context.Context, key, mediaType string, data []byte) error
	Remove(ctx context.Context, key string)
	SignDownload(ctx context.Context, key string, ttl time.Duration) (string, error)
}

type Service struct {
	store   Store
	objects Objects
	slots   chan struct{}
}

func NewService(store Store, objects Objects) *Service {
	return &Service{store: store, objects: objects, slots: make(chan struct{}, 2)}
}

func ValidID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func newID() string {
	var id [16]byte
	rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

func (s *Service) Begin(ctx context.Context, userID, channelID int64, mediaType string, size int64) (UploadForm, error) {
	if size < 1 || size > MaxBytes || (mediaType != "image/jpeg" && mediaType != "image/png") {
		return UploadForm{}, ErrInvalid
	}
	id := newID()
	expires := time.Now().UTC().Add(uploadTTL)
	upload := Upload{Image: Image{ID: id, MediaType: mediaType, SizeBytes: size}, ExpiresAt: expires}
	if err := s.store.Reserve(ctx, userID, channelID, upload); err != nil {
		return UploadForm{}, err
	}
	url, fields, err := s.objects.SignUpload(ctx, "uploads/"+id, mediaType, size, expires)
	if err != nil {
		return UploadForm{}, err
	}
	return UploadForm{ID: id, URL: url, Fields: fields, ExpiresAt: expires}, nil
}

func (s *Service) Complete(ctx context.Context, userID int64, id string) (Image, error) {
	if !ValidID(id) {
		return Image{}, ErrUnavailable
	}
	upload, err := s.store.Owned(ctx, userID, id)
	if err != nil {
		return Image{}, err
	}
	if upload.ObjectKey != "" {
		return upload.Image, nil
	}

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return Image{}, ErrBusy
	}

	stagedKey := "uploads/" + id
	data, err := s.objects.ReadUpload(ctx, stagedKey, upload.SizeBytes)
	if err != nil {
		if errors.Is(err, ErrIncomplete) {
			current, lookupErr := s.store.Owned(ctx, userID, id)
			if lookupErr != nil {
				return Image{}, lookupErr
			}
			if current.ObjectKey != "" {
				return current.Image, nil
			}
		}
		return Image{}, err
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || "image/"+format != upload.MediaType ||
		config.Width < 1 || config.Height < 1 || config.Width > 4096 || config.Height > 4096 ||
		config.Width*config.Height > 12_000_000 {
		return Image{}, ErrIncomplete
	}
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return Image{}, ErrIncomplete
	}
	upload.Width, upload.Height = config.Width, config.Height
	upload.ObjectKey = "images/" + id + "/" + newID()
	if err := s.objects.Save(ctx, upload.ObjectKey, upload.MediaType, data); err != nil {
		return Image{}, err
	}
	ok, err := s.store.Finalize(ctx, userID, upload)
	if err != nil {
		// A lost database response may still mean the object was committed.
		return Image{}, err
	}
	if !ok {
		s.objects.Remove(ctx, upload.ObjectKey)
		winner, err := s.store.Owned(ctx, userID, id)
		if err != nil {
			return Image{}, err
		}
		if winner.ObjectKey == "" {
			return Image{}, ErrUnavailable
		}
		return winner.Image, nil
	}
	s.objects.Remove(ctx, stagedKey)
	return upload.Image, nil
}

func (s *Service) Download(ctx context.Context, userID int64, id string) (Download, error) {
	if !ValidID(id) {
		return Download{}, ErrUnavailable
	}
	upload, err := s.store.Visible(ctx, userID, id)
	if err != nil {
		return Download{}, err
	}
	url, err := s.objects.SignDownload(ctx, upload.ObjectKey, downloadTTL)
	if err != nil {
		return Download{}, err
	}
	return Download{Image: upload.Image, URL: url}, nil
}
