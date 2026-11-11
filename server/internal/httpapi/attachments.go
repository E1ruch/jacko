package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"jacko/server/internal/attachments"
	"jacko/server/internal/telemetry"
)

type attachmentAPI struct {
	service *attachments.Service
}

func RegisterAttachments(mux *http.ServeMux, service *attachments.Service, auth Authenticator, metrics *telemetry.Metrics) {
	api := attachmentAPI{service: service}
	mux.Handle("POST /api/v1/channels/{channel}/attachments", metrics.HTTP("attachment_begin", withUser(auth, api.begin)))
	mux.Handle("POST /api/v1/attachments/{attachment}/complete", metrics.HTTP("attachment_complete", withUser(auth, api.complete)))
	mux.Handle("GET /api/v1/attachments/{attachment}", metrics.HTTP("attachment_download", withUser(auth, api.download)))
}

func (a attachmentAPI) begin(w http.ResponseWriter, r *http.Request, userID int64) {
	channelID, err := strconv.ParseInt(r.PathValue("channel"), 10, 64)
	if err != nil || channelID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_channel")
		return
	}
	var body struct {
		MediaType string `json:"media_type"`
		SizeBytes int64  `json:"size_bytes"`
	}
	if !readJSON(w, r, 4096, &body) {
		return
	}
	result, err := a.service.Begin(r.Context(), userID, channelID, body.MediaType, body.SizeBytes)
	writeAttachmentResult(w, http.StatusCreated, result, err)
}

func (a attachmentAPI) complete(w http.ResponseWriter, r *http.Request, userID int64) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := a.service.Complete(ctx, userID, r.PathValue("attachment"))
	writeAttachmentResult(w, http.StatusOK, result, err)
}

func (a attachmentAPI) download(w http.ResponseWriter, r *http.Request, userID int64) {
	result, err := a.service.Download(r.Context(), userID, r.PathValue("attachment"))
	writeAttachmentResult(w, http.StatusOK, result, err)
}

func writeAttachmentResult(w http.ResponseWriter, status int, result any, err error) {
	switch {
	case err == nil:
		writeJSON(w, status, result)
	case errors.Is(err, attachments.ErrUnavailable):
		writeError(w, http.StatusNotFound, "attachment_not_found")
	case errors.Is(err, attachments.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_attachment")
	case errors.Is(err, attachments.ErrIncomplete):
		writeError(w, http.StatusUnprocessableEntity, "invalid_upload")
	case errors.Is(err, attachments.ErrBusy):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "attachment_busy")
	case errors.Is(err, attachments.ErrStorage):
		slog.Warn("attachment storage request failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "attachment_storage_unavailable")
	default:
		internalError(w, err)
	}
}
