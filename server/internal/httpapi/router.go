package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"jacko/server/internal/messages"
	"jacko/server/internal/sessions"
	"jacko/server/internal/telemetry"
)

type Authenticator interface {
	Authenticate(ctx context.Context, token string) (int64, error)
}

type api struct {
	messages *messages.Service
	auth     Authenticator
}

func New(messageService *messages.Service, auth Authenticator, metrics *telemetry.Metrics) http.Handler {
	api := &api{messages: messageService, auth: auth}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("POST /api/v1/channels/{channel}/messages", metrics.HTTP("message_send", api.withChannel(api.send)))
	mux.Handle("GET /api/v1/channels/{channel}/messages", metrics.HTTP("message_history", api.withChannel(api.history)))
	mux.Handle("PATCH /api/v1/channels/{channel}/messages/{message}", metrics.HTTP("message_edit", api.withChannel(api.edit)))
	mux.Handle("DELETE /api/v1/channels/{channel}/messages/{message}", metrics.HTTP("message_delete", api.withChannel(api.delete)))

	return mux
}

func (a *api) withChannel(next func(http.ResponseWriter, *http.Request, int64, int64)) http.HandlerFunc {
	return withUser(a.auth, func(w http.ResponseWriter, r *http.Request, userID int64) {
		channelID, err := strconv.ParseInt(r.PathValue("channel"), 10, 64)
		if err != nil || channelID <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_channel")
			return
		}
		next(w, r, userID, channelID)
	})
}

func withUser(auth Authenticator, next func(http.ResponseWriter, *http.Request, int64)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		token := bearerToken(r)
		if token == "" {
			unauthorized(w)
			return
		}
		userID, err := auth.Authenticate(r.Context(), token)
		if errors.Is(err, sessions.ErrInvalid) {
			unauthorized(w)
			return
		}
		if err != nil {
			internalError(w, err)
			return
		}
		next(w, r, userID)
	}
}

func (a *api) send(w http.ResponseWriter, r *http.Request, userID, channelID int64) {
	var body struct {
		Nonce        string  `json:"nonce"`
		Content      string  `json:"content"`
		ReplyTo      *int64  `json:"reply_to,string"`
		AttachmentID *string `json:"attachment_id"`
	}
	// Allow for JSON escape sequences in the 8 KiB content field.
	if !readJSON(w, r, 64<<10, &body) {
		return
	}

	message, err := a.messages.Send(r.Context(), messages.SendCommand{
		AuthorID: userID, ChannelID: channelID, Nonce: body.Nonce, Content: body.Content, ReplyTo: body.ReplyTo, AttachmentID: body.AttachmentID,
	})
	if err != nil {
		writeMessageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, message)
}

func (a *api) history(w http.ResponseWriter, r *http.Request, userID, channelID int64) {
	query := messages.HistoryQuery{UserID: userID, ChannelID: channelID, Limit: 50}
	parameters := r.URL.Query()
	if parameters.Has("limit") {
		limit, err := strconv.Atoi(parameters.Get("limit"))
		if err != nil {
			writeMessageError(w, messages.ErrInvalidHistory)
			return
		}
		query.Limit = limit
	}
	if parameters.Has("before") {
		before, err := strconv.ParseInt(parameters.Get("before"), 10, 64)
		if err != nil {
			writeMessageError(w, messages.ErrInvalidHistory)
			return
		}
		query.Before = &before
	}

	page, err := a.messages.History(r.Context(), query)
	if err != nil {
		writeMessageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func bearerToken(r *http.Request) string {
	credentials := strings.Fields(r.Header.Get("Authorization"))
	if len(credentials) != 2 || !strings.EqualFold(credentials[0], "Bearer") {
		return ""
	}
	return credentials[1]
}

func readJSON(w http.ResponseWriter, r *http.Request, limit int64, body any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "expected_json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(body); err != nil {
		invalidBody(w, err)
		return false
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		invalidBody(w, err)
		return false
	}
	return true
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "invalid_session")
}

func invalidBody(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_json")
}

func writeMessageError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, messages.ErrInvalidSend):
		writeError(w, http.StatusBadRequest, "invalid_message")
	case errors.Is(err, messages.ErrInvalidHistory):
		writeError(w, http.StatusBadRequest, "invalid_history")
	case errors.Is(err, messages.ErrChannelAccess):
		writeError(w, http.StatusNotFound, "channel_not_found")
	case errors.Is(err, messages.ErrAttachmentNotFound):
		writeError(w, http.StatusNotFound, "attachment_not_found")
	case errors.Is(err, messages.ErrMessageNotFound):
		writeError(w, http.StatusNotFound, "message_not_found")
	case errors.Is(err, messages.ErrReplyNotFound):
		writeError(w, http.StatusNotFound, "reply_target_not_found")
	case errors.Is(err, messages.ErrNonceConflict):
		writeError(w, http.StatusConflict, "nonce_conflict")
	default:
		internalError(w, err)
	}
}

func internalError(w http.ResponseWriter, err error) {
	slog.Error("request failed", "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error")
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"code": code})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("response write failed", "error", err)
	}
}
