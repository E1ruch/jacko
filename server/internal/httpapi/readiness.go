package httpapi

import (
	"context"
	"net/http"
	"time"
)

// Readiness checks the durable store. Redis and S3 failures do not prevent text
// messages from being stored, so they must not take the whole API out of service.
func Readiness(check func(context.Context) error) http.HandlerFunc {
	return restHandler(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := check(ctx); err != nil {
			writeError(w, http.StatusServiceUnavailable, "not_ready")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
