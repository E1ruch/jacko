package httpapi

import (
	"errors"
	"net/http"
	"time"

	"jacko/server/internal/accounts"
	"jacko/server/internal/sessions"
	"jacko/server/internal/telemetry"
)

type authAPI struct {
	accounts *accounts.Service
	sessions *sessions.Service
	delivery *accounts.Delivery
}

func RegisterAuth(mux *http.ServeMux, accountService *accounts.Service, sessionService *sessions.Service, metrics *telemetry.Metrics) {
	RegisterAuthWithOptions(mux, accountService, sessionService, metrics, nil, nil)
}

func RegisterAuthWithOptions(mux *http.ServeMux, accountService *accounts.Service, sessionService *sessions.Service, metrics *telemetry.Metrics, limits RateLimiter, delivery *accounts.Delivery) {
	auth := authAPI{accounts: accountService, sessions: sessionService, delivery: delivery}
	passwordAttempts := newAttemptLimiter(10, 6*time.Second)
	refreshAttempts := newAttemptLimiter(60, time.Second)
	mux.Handle("POST /api/v1/auth/register", authRoute(metrics, "auth_register", passwordAttempts.wrapShared(limits, "password", auth.register)))
	mux.Handle("POST /api/v1/auth/login", authRoute(metrics, "auth_login", passwordAttempts.wrapShared(limits, "password", auth.login)))
	mux.Handle("POST /api/v1/auth/refresh", authRoute(metrics, "auth_refresh", refreshAttempts.wrapShared(limits, "refresh", auth.refresh)))
	mux.Handle("GET /api/v1/auth/session", authRoute(metrics, "auth_session", auth.session))
	mux.Handle("DELETE /api/v1/auth/session", authRoute(metrics, "auth_logout", auth.logout))
	if delivery != nil {
		emailAttempts := newAttemptLimiter(5, 12*time.Second)
		mux.Handle("POST /api/v1/auth/email/resend", authRoute(metrics, "email_resend", emailAttempts.wrapShared(limits, "email", auth.requestCode(accounts.VerifyEmail))))
		mux.Handle("POST /api/v1/auth/email/verify", authRoute(metrics, "email_verify", emailAttempts.wrapShared(limits, "email", auth.verifyEmail)))
		mux.Handle("POST /api/v1/auth/password/forgot", authRoute(metrics, "password_forgot", emailAttempts.wrapShared(limits, "email", auth.requestCode(accounts.ResetPassword))))
		mux.Handle("POST /api/v1/auth/password/reset", authRoute(metrics, "password_reset", passwordAttempts.wrapShared(limits, "password", auth.resetPassword)))
	}

}

func authRoute(metrics *telemetry.Metrics, name string, handler http.HandlerFunc) http.Handler {
	return metrics.HTTP(name, restHandler(handler))
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
	if a.delivery != nil {
		a.delivery.Submit(body.Email, accounts.VerifyEmail)
	}
	writeJSON(w, http.StatusCreated, accountID{UserID: userID})
}

func (a authAPI) login(w http.ResponseWriter, r *http.Request) {
	var body credentials
	if !readJSON(w, r, 4096, &body) {
		return
	}
	credential, err := a.accounts.Verify(r.Context(), body.Email, body.Password)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if a.delivery != nil && !credential.EmailVerified {
		writeError(w, http.StatusForbidden, "email_not_verified")
		return
	}
	tokens, err := a.sessions.IssueForPassword(r.Context(), credential.UserID, credential.PasswordVersion)
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
	case errors.Is(err, accounts.ErrInvalidCode):
		writeError(w, http.StatusBadRequest, "invalid_account_code")
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
