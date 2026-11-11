package httpapi

import (
	"errors"
	"net/http"

	"jacko/server/internal/accounts"
	"jacko/server/internal/sessions"
	"jacko/server/internal/telemetry"
)

type authAPI struct {
	accounts *accounts.Service
	sessions *sessions.Service
}

func RegisterAuth(mux *http.ServeMux, accountService *accounts.Service, sessionService *sessions.Service, metrics *telemetry.Metrics) {
	auth := authAPI{accounts: accountService, sessions: sessionService}
	mux.Handle("POST /api/v1/auth/register", authRoute(metrics, "auth_register", auth.register))
	mux.Handle("POST /api/v1/auth/login", authRoute(metrics, "auth_login", auth.login))
	mux.Handle("POST /api/v1/auth/refresh", authRoute(metrics, "auth_refresh", auth.refresh))
	mux.Handle("GET /api/v1/auth/session", authRoute(metrics, "auth_session", auth.session))
	mux.Handle("DELETE /api/v1/auth/session", authRoute(metrics, "auth_logout", auth.logout))
}

func authRoute(metrics *telemetry.Metrics, name string, handler http.HandlerFunc) http.Handler {
	return metrics.HTTP(name, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		handler(w, r)
	}))
}

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type accountID struct {
	UserID int64 `json:"user_id,string"`
}

func (a authAPI) register(w http.ResponseWriter, r *http.Request) {
	var body credentials
	if !readJSON(w, r, 4096, &body) {
		return
	}
	userID, err := a.accounts.Register(r.Context(), body.Email, body.Password)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, accountID{UserID: userID})
}

func (a authAPI) login(w http.ResponseWriter, r *http.Request) {
	var body credentials
	if !readJSON(w, r, 4096, &body) {
		return
	}
	userID, err := a.accounts.Verify(r.Context(), body.Email, body.Password)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	tokens, err := a.sessions.Issue(r.Context(), userID)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

func (a authAPI) refresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !readJSON(w, r, 4096, &body) {
		return
	}
	tokens, err := a.sessions.Refresh(r.Context(), body.RefreshToken)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

func (a authAPI) session(w http.ResponseWriter, r *http.Request) {
	userID, err := a.sessions.Authenticate(r.Context(), bearerToken(r))
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, accountID{UserID: userID})
}

func (a authAPI) logout(w http.ResponseWriter, r *http.Request) {
	if err := a.sessions.Revoke(r.Context(), bearerToken(r)); err != nil {
		writeAuthError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, accounts.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_account")
	case errors.Is(err, accounts.ErrEmailTaken):
		writeError(w, http.StatusConflict, "email_already_registered")
	case errors.Is(err, accounts.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, "invalid_credentials")
	case errors.Is(err, accounts.ErrBusy):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "auth_busy")
	case errors.Is(err, sessions.ErrInvalid):
		unauthorized(w)
	default:
		internalError(w, err)
	}
}
