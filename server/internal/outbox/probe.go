package outbox

import (
	"context"
	"log/slog"
	"time"
)

// ObserveBacklog samples the shared database independently of scrapes and of publication.
func (w *Worker) ObserveBacklog(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		sample, cancel := context.WithTimeout(ctx, time.Second)
		var age float64
		err := w.pool.QueryRow(sample, `
			SELECT coalesce(extract(epoch FROM clock_timestamp() - min(created_at)), 0)::double precision
			FROM message_outbox
			WHERE published_at IS NULL
		`).Scan(&age)
		cancel()

		if err != nil {
			w.metrics.OutboxProbeOK.Set(0)
			if ctx.Err() == nil {
				slog.Warn("outbox backlog sample failed", "error", err)
			}
		} else {
			w.metrics.OutboxOldestPending.Set(max(age, 0))
			w.metrics.OutboxProbeOK.Set(1)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
