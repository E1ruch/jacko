package postgres_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"jacko/server/internal/accounts"
	accountpg "jacko/server/internal/accounts/postgres"
	"jacko/server/internal/httpapi"
	"jacko/server/internal/sessions"
	sessionpg "jacko/server/internal/sessions/postgres"
	"jacko/server/internal/telemetry"
)

func testAuth(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	mux := http.NewServeMux()
	httpapi.RegisterAuth(mux, accounts.NewService(accountpg.NewStore(pool)),
		sessions.NewService(sessionpg.NewStore(pool)), telemetry.New())

	call := func(method, route, body, access string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "/api/v1/auth/"+route, strings.NewReader(body)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		if access != "" {
			request.Header.Set("Authorization", "Bearer "+access)
		}
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		return response
	}
	check := func(response *httptest.ResponseRecorder, want int) {
		t.Helper()
		if response.Code != want || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("status %d, want %d with no-store", response.Code, want)
		}
	}
	decodeTokens := func(response *httptest.ResponseRecorder) sessions.Tokens {
		t.Helper()
		var tokens sessions.Tokens
		if err := json.Unmarshal(response.Body.Bytes(), &tokens); err != nil {
			t.Fatal(err)
		}
		if tokens.UserID <= 0 || len(tokens.AccessToken) != 64 || len(tokens.RefreshToken) != 64 ||
			tokens.AccessToken == tokens.RefreshToken || !tokens.ExpiresAt.After(time.Now()) ||
			!tokens.RefreshExpiresAt.After(tokens.ExpiresAt) {
			t.Fatal("invalid issued tokens")
		}
		return tokens
	}
	refreshBody := func(token string) string {
		return `{"refresh_token":"` + token + `"}`
	}

	const credentials = `{"email":" Demo@jacko.test ","password":"long-enough-test-password"}`
	check(call("POST", "register", `{"email":"demo@jacko.test","password":"short"}`, ""), 400)
	registered := call("POST", "register", credentials, "")
	check(registered, 201)
	var account struct {
		UserID int64 `json:"user_id,string"`
	}
	if err := json.Unmarshal(registered.Body.Bytes(), &account); err != nil || account.UserID <= 0 {
		t.Fatal("registration did not return a user ID")
	}
	check(call("POST", "register", `{"email":"demo@jacko.test","password":"long-enough-test-password"}`, ""), 409)
	check(call("POST", "login", `{"email":"demo@jacko.test","password":"incorrect-password"}`, ""), 401)
	check(call("POST", "login", `{"email":"unknown@jacko.test","password":"incorrect-password"}`, ""), 401)

	login := call("POST", "login", credentials, "")
	check(login, 200)
	original := decodeTokens(login)
	if original.UserID != account.UserID {
		t.Fatal("session belongs to a different user")
	}
	check(call("GET", "session", "", original.AccessToken), 200)
	check(call("GET", "session", "", original.RefreshToken), 401)
	check(call("POST", "refresh", refreshBody(original.AccessToken), ""), 401)

	var attempts [6]*httptest.ResponseRecorder
	var group sync.WaitGroup
	for i := range attempts {
		group.Go(func() {
			attempts[i] = call("POST", "refresh", refreshBody(original.RefreshToken), "")
		})
	}
	group.Wait()
	var rotated sessions.Tokens
	successes := 0
	for _, response := range attempts {
		if response.Code == 200 {
			check(response, 200)
			rotated = decodeTokens(response)
			successes++
		} else {
			check(response, 401)
		}
	}
	if successes != 1 || rotated.AccessToken == original.AccessToken ||
		rotated.RefreshToken == original.RefreshToken || !rotated.RefreshExpiresAt.Equal(original.RefreshExpiresAt) {
		t.Fatal("concurrent refresh did not rotate exactly one token pair")
	}
	check(call("GET", "session", "", original.AccessToken), 401)
	check(call("GET", "session", "", rotated.AccessToken), 200)

	secondLogin := call("POST", "login", credentials, "")
	check(secondLogin, 200)
	second := decodeTokens(secondLogin)
	check(call("DELETE", "session", "", rotated.AccessToken), 204)
	check(call("DELETE", "session", "", rotated.AccessToken), 204)
	check(call("GET", "session", "", rotated.AccessToken), 401)
	check(call("POST", "refresh", refreshBody(rotated.RefreshToken), ""), 401)
	check(call("GET", "session", "", second.AccessToken), 200)
}
