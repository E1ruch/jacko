package httpapi

import (
	"net/http"

	"jacko/server/internal/accounts"
)

func (a authAPI) requestCode(purpose accounts.Purpose) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email string `json:"email"`
		}
		if !readJSON(w, r, 4096, &body) {
			return
		}
		if !a.delivery.Submit(body.Email, purpose) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusServiceUnavailable, "email_busy")
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

func (a authAPI) verifyEmail(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if !readJSON(w, r, 4096, &body) {
		return
	}
	if err := a.accounts.ConfirmEmail(r.Context(), body.Code); err != nil {
		writeAuthError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a authAPI) resetPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code     string `json:"code"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, 4096, &body) {
		return
	}
	if err := a.accounts.ResetPassword(r.Context(), body.Code, body.Password); err != nil {
		writeAuthError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
