package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConnectionCapacity(t *testing.T) {
	gateway := New(context.Background(), nil, nil, nil, nil, nil)
	gateway.slots = make(chan struct{}, 1)
	gateway.slots <- struct{}{}
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequest("GET", "/", nil))
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") == "" {
		t.Fatalf("full gateway returned %d", response.Code)
	}
	<-gateway.slots
	response = httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequest("GET", "/", nil))
	if response.Code != http.StatusBadRequest || len(gateway.slots) != 0 {
		t.Fatal("failed handshake leaked a connection slot")
	}
}
