package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"
	"unicode/utf8"
)

type Purpose string

const (
	VerifyEmail   Purpose = "verify_email"
	ResetPassword Purpose = "reset_password"
)

var ErrInvalidCode = errors.New("invalid account code")

type Mailer interface {
	Send(ctx context.Context, recipient string, purpose Purpose, code string) error
}

type deliveryJob struct {
	email   string
	purpose Purpose
}

type Delivery struct {
	service *Service
	mailer  Mailer
	jobs    chan deliveryJob
}

func NewDelivery(service *Service, mailer Mailer) *Delivery {
	return &Delivery{service: service, mailer: mailer, jobs: make(chan deliveryJob, 128)}
}

func (d *Delivery) Submit(email string, purpose Purpose) bool {
	select {
	case d.jobs <- deliveryJob{email, purpose}:
		return true
	default:
		return false
	}
}

func (d *Delivery) Run(ctx context.Context) {
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case job := <-d.jobs:
			request, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := d.deliver(request, job); err != nil {
				// SMTP errors may contain recipient or message data.
				slog.Warn("account email delivery failed", "purpose", job.purpose)
			}
			cancel()
		}
	}
}

func (d *Delivery) deliver(ctx context.Context, job deliveryJob) error {
	email, valid := normalizeEmail(job.email)
	if !valid {
		return nil
	}
	var secret [32]byte
	rand.Read(secret[:])
	saved, err := d.service.store.SaveCode(ctx, email, job.purpose, sha256.Sum256(secret[:]))
	if err != nil || !saved {
		return err
	}
	return d.mailer.Send(ctx, email, job.purpose, hex.EncodeToString(secret[:]))
}

func (s *Service) ConfirmEmail(ctx context.Context, token string) error {
	hash, err := codeHash(token)
	if err != nil {
		return err
	}
	return s.store.ConsumeCode(ctx, VerifyEmail, hash, Credential{})
}

func (s *Service) ResetPassword(ctx context.Context, token, password string) error {
	hash, err := codeHash(token)
	if err != nil {
		return err
	}
	if !validPassword(password) || utf8.RuneCountInString(password) < 12 {
		return ErrInvalidInput
	}
	var credential Credential
	rand.Read(credential.Salt[:])
	derived, err := s.derive(ctx, password, credential.Salt)
	if err != nil {
		return err
	}
	copy(credential.Hash[:], derived)
	return s.store.ConsumeCode(ctx, ResetPassword, hash, credential)
}

func codeHash(token string) ([32]byte, error) {
	var secret [32]byte
	if len(token) != hex.EncodedLen(len(secret)) {
		return [32]byte{}, ErrInvalidCode
	}
	if _, err := hex.Decode(secret[:], []byte(token)); err != nil {
		return [32]byte{}, ErrInvalidCode
	}
	return sha256.Sum256(secret[:]), nil
}
