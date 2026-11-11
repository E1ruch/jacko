// Package outbox publishes committed message events.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jacko/server/internal/telemetry"
)

const batchSize = 32

type Publisher interface {
	Publish(ctx context.Context, payload []byte) error
}

type Worker struct {
	pool      *pgxpool.Pool
	publisher Publisher
	metrics   *telemetry.Metrics
}

func NewWorker(pool *pgxpool.Pool, publisher Publisher, metrics *telemetry.Metrics) *Worker {
	return &Worker{pool: pool, publisher: publisher, metrics: metrics}
}

func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		batchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		start := time.Now()
		count, err := w.flush(batchCtx)
		cancel()

		delay := 25 * time.Millisecond
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.metrics.OutboxFailures.Inc()
			w.metrics.OutboxBatchDuration.WithLabelValues("error").Observe(time.Since(start).Seconds())
			slog.Warn("outbox publication failed", "error", err)
			delay = time.Second
		} else if count > 0 {
			w.metrics.OutboxPublished.Add(float64(count))
			w.metrics.OutboxBatchDuration.WithLabelValues("ok").Observe(time.Since(start).Seconds())
			if count == batchSize {
				continue
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (w *Worker) flush(ctx context.Context) (int, error) {
	count := 0
	err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, channel_seq, event_type, payload
			FROM message_outbox
			WHERE published_at IS NULL
			ORDER BY id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		`, batchSize)
		if err != nil {
			return fmt.Errorf("select outbox: %w", err)
		}
		events, err := pgx.CollectRows(rows, scanEvent)
		if err != nil {
			return fmt.Errorf("read outbox: %w", err)
		}
		count = len(events)
		if count == 0 {
			return nil
		}
		ids := make([]int64, 0, count)
		for _, item := range events {
			payload, err := json.Marshal(item)
			if err != nil {
				return fmt.Errorf("encode outbox event %d: %w", item.ID, err)
			}
			if err := w.publisher.Publish(ctx, payload); err != nil {
				return fmt.Errorf("publish outbox event %d: %w", item.ID, err)
			}
			ids = append(ids, item.ID)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE message_outbox SET published_at = clock_timestamp() WHERE id = ANY($1)
		`, ids); err != nil {
			return fmt.Errorf("mark outbox published: %w", err)
		}
		return nil
	})
	return count, err
}
