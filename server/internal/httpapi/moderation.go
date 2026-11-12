package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"jacko/server/internal/moderation"
	"jacko/server/internal/telemetry"
)

func RegisterModeration(mux *http.ServeMux, store *moderation.Store, auth Authenticator, metrics *telemetry.Metrics, limits RateLimiter) {
	reportLimit := newAttemptLimiter(10, 6*time.Second)
	mux.Handle("POST /api/v1/channels/{channel}/messages/{message}/reports", metrics.HTTP("report_create", reportLimit.wrapShared(limits, "reports", withUser(auth, func(w http.ResponseWriter, r *http.Request, user int64) {
		channel, validChannel := positivePath(r, "channel")
		message, validMessage := positivePath(r, "message")
		if !validChannel || !validMessage {
			writeError(w, http.StatusBadRequest, "invalid_message_id")
			return
		}
		var body struct {
			Reason string `json:"reason"`
		}
		if !readJSON(w, r, 8192, &body) {
			return
		}
		body.Reason = strings.TrimSpace(body.Reason)
		if body.Reason == "" || len(body.Reason) > 1000 || !utf8.ValidString(body.Reason) || strings.ContainsRune(body.Reason, 0) {
			writeError(w, http.StatusBadRequest, "invalid_report_reason")
			return
		}
		if err := store.Create(r.Context(), user, channel, message, body.Reason); err != nil {
			writeReportError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))))
	mux.Handle("GET /api/v1/guilds/{guild}/reports", metrics.HTTP("report_list", withUser(auth, func(w http.ResponseWriter, r *http.Request, user int64) {
		guild, ok := guildParameter(w, r)
		if !ok {
			return
		}
		after, ok := afterCursor(w, r)
		if !ok {
			return
		}
		items, err := store.List(r.Context(), user, guild, after)
		if err != nil {
			writeReportError(w, err)
			return
		}
		page := resourcePage[moderation.Report]{Items: items}
		if len(items) > moderation.PageSize {
			page.Items = items[:moderation.PageSize]
			page.NextAfter = page.Items[len(page.Items)-1].ID
		}
		writeJSON(w, http.StatusOK, page)
	})))
	mux.Handle("PATCH /api/v1/guilds/{guild}/reports/{report}", metrics.HTTP("report_resolve", withUser(auth, func(w http.ResponseWriter, r *http.Request, user int64) {
		guild, ok := guildParameter(w, r)
		if !ok {
			return
		}
		report, ok := positivePath(r, "report")
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_report_id")
			return
		}
		var body struct {
			Resolution string `json:"resolution"`
		}
		if !readJSON(w, r, 4096, &body) {
			return
		}
		if body.Resolution != "reviewed" && body.Resolution != "dismissed" {
			writeError(w, http.StatusBadRequest, "invalid_report_resolution")
			return
		}
		if err := store.Resolve(r.Context(), user, guild, report, body.Resolution); err != nil {
			writeReportError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
}

func positivePath(r *http.Request, field string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(field), 10, 64)
	return id, err == nil && id > 0
}

func writeReportError(w http.ResponseWriter, err error) {
	if errors.Is(err, moderation.ErrUnavailable) {
		writeError(w, http.StatusNotFound, "resource_not_found")
		return
	}
	internalError(w, err)
}
