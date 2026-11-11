package postgres_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"jacko/server/internal/messages"
)

func TestHistoryWithTombstones(t *testing.T) {
	a := newResourceAPI(t)
	guild := a.guild(t)
	_, err := a.pool.Exec(a.ctx, `
		INSERT INTO messages (channel_id, author_id, nonce, content, original_content_hash, deleted_at)
		SELECT $1, $2, 'history-' || n,
		       CASE WHEN n <= 50 THEN 'live' END,
		       sha256(convert_to('live', 'UTF8')),
		       CASE WHEN n > 50 THEN now() END
		FROM generate_series(1, 20000) AS n
	`, guild.Channel.ID, a.owner.UserID)
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, a.ctx, a.pool, "ANALYZE messages")
	// This schema is private to this test. Compare actual plans on the same data.
	execSQL(t, a.ctx, a.pool, "DROP INDEX messages_live_history_idx")
	plan := func() ([]byte, float64, int) {
		t.Helper()
		var raw []byte
		err := a.pool.QueryRow(a.ctx, `
			EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
			SELECT id FROM messages
			WHERE channel_id = $1 AND deleted_at IS NULL
			ORDER BY id DESC LIMIT 50
		`, guild.Channel.ID).Scan(&raw)
		if err != nil {
			t.Fatal(err)
		}
		var result []struct {
			Time float64 `json:"Execution Time"`
			Plan struct {
				Hits  int `json:"Shared Hit Blocks"`
				Reads int `json:"Shared Read Blocks"`
			} `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &result); err != nil || len(result) != 1 {
			t.Fatalf("invalid query plan: %s, %v", raw, err)
		}
		return raw, result[0].Time, result[0].Plan.Hits + result[0].Plan.Reads
	}
	_, beforeMS, beforeBuffers := plan()
	applyMigration(t, a.ctx, a.pool, "000009_live_history_index.up.sql")
	afterPlan, afterMS, afterBuffers := plan()
	if !strings.Contains(string(afterPlan), "messages_live_history_idx") || afterBuffers >= beforeBuffers {
		t.Fatalf("live history did not avoid tombstones: buffers %d -> %d; %s", beforeBuffers, afterBuffers, afterPlan)
	}
	page := decodeResource[messages.HistoryPage](t, a.call(t, "GET",
		fmt.Sprintf("/api/v1/channels/%d/messages", guild.Channel.ID), a.owner.AccessToken, "", 200))
	if len(page.Messages) != 50 || page.NextBefore != 0 {
		t.Fatal("history included deleted messages or omitted live messages")
	}
	t.Logf("20,000 rows, 19,950 tombstones: history plan %.3fms -> %.3fms; buffer accesses %d -> %d",
		beforeMS, afterMS, beforeBuffers, afterBuffers)
}
