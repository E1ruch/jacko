package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jacko/server/internal/telemetry"
)

func TestAttemptLimiter(t *testing.T) {
	now := time.Now()
	peer := netip.MustParseAddr("192.0.2.1")
	limiter := newAttemptLimiter(10, 6*time.Second)
	var allowed atomic.Int32
	var group sync.WaitGroup
	for range 100 {
		group.Go(func() {
			if limiter.allow(peer, now) {
				allowed.Add(1)
			}
		})
	}
	group.Wait()
	if allowed.Load() != 10 {
		t.Fatalf("concurrent burst allowed %d requests, want 10", allowed.Load())
	}
	if limiter.allow(peer, now.Add(5*time.Second)) || !limiter.allow(peer, now.Add(6*time.Second)) {
		t.Fatal("bucket did not refill at the configured rate")
	}
	limiter.maxEntries = 1
	other := netip.MustParseAddr("192.0.2.2")
	if limiter.allow(other, now.Add(7*time.Second)) || len(limiter.buckets) != 1 {
		t.Fatal("limiter exceeded its memory bound or evicted an active bucket")
	}
	if !limiter.allow(other, now.Add(2*time.Minute)) || len(limiter.buckets) != 1 {
		t.Fatal("idle buckets were not reclaimed")
	}
}

func TestAuthRateLimitIgnoresForwardedHeaders(t *testing.T) {
	mux := http.NewServeMux()
	// An invalid body must be rejected before dereferencing either service.
	RegisterAuth(mux, nil, nil, telemetry.New())
	for i := range 11 {
		path := "/api/v1/auth/login"
		if i%2 == 0 {
			path = "/api/v1/auth/register"
		}
		request := httptest.NewRequest("POST", path, strings.NewReader("{}"))
		request.RemoteAddr = "192.0.2.1:1234"
		if i%2 == 0 {
			request.RemoteAddr = "[::ffff:192.0.2.1]:5678"
		}
		request.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i))
		request.Header.Set("X-Real-IP", fmt.Sprintf("198.51.100.%d", i))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		want := http.StatusUnsupportedMediaType
		if i == 10 {
			want = http.StatusTooManyRequests
			if response.Header().Get("Retry-After") == "" {
				t.Fatal("rate limit response omitted Retry-After")
			}
		}
		if response.Code != want || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("attempt %d: got %d, want %d", i, response.Code, want)
		}
	}
}

func TestRESTDeadline(t *testing.T) {
	var captured context.Context
	handler := restHandler(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Context()
		deadline, ok := captured.Deadline()
		if !ok || time.Until(deadline) > requestTimeout {
			t.Fatal("request has no bounded deadline")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/", nil))
	if captured.Err() != context.Canceled || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("request context or response headers were not cleaned up")
	}

	parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	response = httptest.NewRecorder()
	restHandler(func(w http.ResponseWriter, r *http.Request) {
		internalError(w, fmt.Errorf("database: %w", r.Context().Err()))
	}).ServeHTTP(response, httptest.NewRequest("GET", "/", nil).WithContext(parent))
	if response.Code != http.StatusGatewayTimeout || !strings.Contains(response.Body.String(), "request_timeout") {
		t.Fatalf("expired request: %d %s", response.Code, response.Body)
	}
}

func TestReadJSONBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body, media string
		status            int
	}{
		{"valid", `{"name":"Jacko"}`, "application/json; charset=utf-8", 204},
		{"unknown field", `{"name":"Jacko","admin":true}`, "application/json", 400},
		{"trailing value", `{"name":"Jacko"} {}`, "application/json", 400},
		{"too large", `{"name":"` + strings.Repeat("a", 64) + `"}`, "application/json", 413},
		{"wrong media type", `{}`, "text/plain", 415},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", tc.media)
			response := httptest.NewRecorder()
			var body struct {
				Name string `json:"name"`
			}
			if readJSON(response, request, 64, &body) {
				response.WriteHeader(http.StatusNoContent)
			}
			if response.Code != tc.status {
				t.Fatalf("status %d, want %d", response.Code, tc.status)
			}
		})
	}
}

func TestReadiness(t *testing.T) {
	for _, healthy := range []bool{true, false} {
		response := httptest.NewRecorder()
		Readiness(func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > time.Second {
				t.Fatal("readiness check is unbounded")
			}
			if !healthy {
				return errors.New("private database details")
			}
			return nil
		}).ServeHTTP(response, httptest.NewRequest("GET", "/readyz", nil))
		want := http.StatusServiceUnavailable
		if healthy {
			want = http.StatusNoContent
		}
		if response.Code != want || strings.Contains(response.Body.String(), "private") {
			t.Fatalf("readiness leaked details or returned %d, want %d", response.Code, want)
		}
	}
}
