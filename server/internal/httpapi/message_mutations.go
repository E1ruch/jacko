package httpapi

import (
	"net/http"
	"strconv"

	"jacko/server/internal/messages"
)

func (a *api) edit(w http.ResponseWriter, r *http.Request, userID, channelID int64) {
	target, ok := messageTarget(w, r, userID, channelID)
	if !ok {
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if !readJSON(w, r, 64<<10, &body) {
		return
	}
	message, err := a.messages.Edit(r.Context(), target, body.Content)
	if err != nil {
		writeMessageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, message)
}

func (a *api) delete(w http.ResponseWriter, r *http.Request, userID, channelID int64) {
	target, ok := messageTarget(w, r, userID, channelID)
	if !ok {
		return
	}
	if err := a.messages.Delete(r.Context(), target); err != nil {
		writeMessageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func messageTarget(w http.ResponseWriter, r *http.Request, userID, channelID int64) (messages.OwnedMessage, bool) {
	id, err := strconv.ParseInt(r.PathValue("message"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_message_id")
		return messages.OwnedMessage{}, false
	}
	return messages.OwnedMessage{UserID: userID, ChannelID: channelID, MessageID: id}, true
}
