package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jacko/server/internal/httpapi"
	"jacko/server/internal/messages"
	"jacko/server/internal/messages/postgres"
	"jacko/server/internal/outbox"
	"jacko/server/internal/sessions"
	sessionpg "jacko/server/internal/sessions/postgres"
	"jacko/server/internal/telemetry"
)

func TestCreateTransaction(t *testing.T) {
	dsn := os.Getenv("JACKO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set JACKO_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()

	schema := fmt.Sprintf("jacko_test_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	execSQL(t, ctx, admin, "CREATE SCHEMA "+identifier)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		execSQL(t, cleanup, admin, "DROP SCHEMA "+identifier+" CASCADE")
	}()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	applyMigration(t, ctx, pool, "000001_messaging.up.sql")
	applyMigration(t, ctx, pool, "000002_sessions.up.sql")
	execSQL(t, ctx, pool, `
		INSERT INTO users DEFAULT VALUES;
		INSERT INTO users DEFAULT VALUES;
		INSERT INTO guilds (name) VALUES ('Jacko');
		INSERT INTO members (guild_id, user_id) VALUES (1, 1);
		INSERT INTO channels (guild_id, name) VALUES (1, 'general');
	`)
	execSQL(t, ctx, pool, `
		INSERT INTO channels (id, guild_id, name)
		OVERRIDING SYSTEM VALUE VALUES (1000, 1, 'legacy');

		WITH legacy AS (
			INSERT INTO messages (channel_id, author_id, nonce, content)
			VALUES (1000, 1, 'legacy-first', 'first'), (1000, 1, 'legacy-second', 'second')
			RETURNING *
		)
		INSERT INTO message_outbox (message_id, event_type, payload, published_at)
		SELECT id, 'MESSAGE_CREATE', jsonb_build_object('id', id::text, 'channel_id', channel_id::text),
			CASE WHEN nonce = 'legacy-first' THEN now() END
		FROM legacy
		ORDER BY id;
	`)
	applyMigration(t, ctx, pool, "000003_channel_event_cursor.up.sql")
	applyMigration(t, ctx, pool, "000004_outbox_pending_age.up.sql")
	applyMigration(t, ctx, pool, "000005_accounts_and_refresh.up.sql")

	service := messages.NewService(postgres.NewStore(pool))
	t.Run("CursorMigration", func(t *testing.T) {
		const channelID = 1000
		journal := outbox.NewJournal(pool)
		head, err := journal.Head(ctx, 1, channelID)
		if err != nil || head != 2 {
			t.Fatalf("legacy cursor: got %d, %v", head, err)
		}
		if _, err := service.Send(ctx, messages.SendCommand{
			AuthorID: 1, ChannelID: channelID, Nonce: "after-migration", Content: "third",
		}); err != nil {
			t.Fatal(err)
		}
		events, err := journal.Read(ctx, channelID, 0, 3)
		if err != nil || len(events) != 3 || events[0].Seq != 1 || events[1].Seq != 2 || events[2].Seq != 3 {
			t.Fatalf("events after migration: %+v, %v", events, err)
		}
		var published int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM message_outbox WHERE channel_id = $1 AND published_at IS NOT NULL
		`, channelID).Scan(&published); err != nil || published != 1 {
			t.Fatalf("published events after migration: %d, %v", published, err)
		}
	})
	command := messages.SendCommand{AuthorID: 1, ChannelID: 1, Nonce: "first", Content: "Привет"}

	var group sync.WaitGroup
	var sent [8]messages.Message
	var sendErr [8]error
	for i := range sent {
		group.Go(func() {
			sent[i], sendErr[i] = service.Send(ctx, command)
		})
	}
	group.Wait()
	for i, message := range sent {
		if sendErr[i] != nil || message.ID == 0 || message.ID != sent[0].ID {
			t.Fatalf("send %d: got id %d and error %v, first id %d", i, message.ID, sendErr[i], sent[0].ID)
		}
	}

	var outboxID string
	if err := pool.QueryRow(ctx, "SELECT payload->>'id' FROM message_outbox WHERE channel_id = 1").Scan(&outboxID); err != nil {
		t.Fatal(err)
	}
	if outboxID != fmt.Sprint(sent[0].ID) {
		t.Fatalf("outbox payload has message ID %s, want %d", outboxID, sent[0].ID)
	}

	changed := command
	changed.Content = "Другое сообщение"
	if _, err := service.Send(ctx, changed); !errors.Is(err, messages.ErrNonceConflict) {
		t.Fatalf("nonce conflict: got %v", err)
	}
	outsider := command
	outsider.AuthorID = 2
	if _, err := service.Send(ctx, outsider); !errors.Is(err, messages.ErrChannelAccess) {
		t.Fatalf("access check: got %v", err)
	}

	execSQL(t, ctx, pool, `
		ALTER TABLE message_outbox
		ADD CONSTRAINT reject_new_events CHECK (false) NOT VALID
	`)
	command.Nonce = "must-roll-back"
	if _, err := service.Send(ctx, command); err == nil {
		t.Fatal("expected outbox insert to fail")
	}

	var messageCount, eventCount, head int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM messages WHERE channel_id = 1),
		       (SELECT count(*) FROM message_outbox WHERE channel_id = 1),
		       (SELECT last_seq FROM channel_event_heads WHERE channel_id = 1)
	`).Scan(&messageCount, &eventCount, &head); err != nil {
		t.Fatal(err)
	}
	if messageCount != 1 || eventCount != 1 || head != 1 {
		t.Fatalf("unexpected state after rollback: messages=%d, events=%d, head=%d", messageCount, eventCount, head)
	}
	execSQL(t, ctx, pool, "ALTER TABLE message_outbox DROP CONSTRAINT reject_new_events")
	t.Run("HTTP", func(t *testing.T) {
		testHTTP(t, ctx, pool, service, sent[0].ID)
	})
	t.Run("Auth", func(t *testing.T) {
		testAuth(t, ctx, pool)
	})
	t.Run("Realtime", func(t *testing.T) {
		testRealtime(t, ctx, pool, service, schema)
	})
}

func applyMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()
	migration, err := os.ReadFile("../../../migrations/" + name)
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, ctx, pool, string(migration))
}

func testHTTP(t *testing.T, ctx context.Context, pool *pgxpool.Pool, service *messages.Service, firstID int64) {
	t.Helper()
	for _, session := range []struct {
		secret byte
		userID int64
		active bool
	}{{0, 1, true}, {1, 2, true}, {2, 1, false}, {3, 1, true}} {
		var secret [32]byte
		for i := range secret {
			secret[i] = session.secret
		}
		hash := sha256.Sum256(secret[:])
		expiry := time.Now().Add(time.Hour)
		if !session.active {
			expiry = time.Now().Add(-time.Hour)
		}
		if _, err := pool.Exec(ctx, "INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)", hash[:], session.userID, expiry); err != nil {
			t.Fatal(err)
		}
	}
	execSQL(t, ctx, pool, "INSERT INTO channels (guild_id, name) VALUES (1, 'empty')")
	execSQL(t, ctx, pool, "UPDATE sessions SET revoked_at = now() WHERE user_id = 1 AND token_hash = sha256(decode(repeat('03', 32), 'hex'))")
	handler := httpapi.New(service, sessions.NewService(sessionpg.NewStore(pool)), telemetry.New())
	const route = "/api/v1/channels/1/messages"
	member := strings.Repeat("00", 32)
	outsider := strings.Repeat("01", 32)
	expired := strings.Repeat("02", 32)
	revoked := strings.Repeat("03", 32)
	call := func(method, path, token, body string, want int) []byte {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != want {
			t.Fatalf("%s %s: status %d, want %d, body %s", method, path, response.Code, want, response.Body)
		}
		return response.Body.Bytes()
	}

	call("GET", route, "", "", 401)
	call("GET", route, expired, "", 401)
	call("GET", route, revoked, "", 401)
	call("GET", route, outsider, "", 404)
	call("GET", route+"?limit=101", member, "", 400)
	empty := call("GET", "/api/v1/channels/2/messages", member, "", 200)
	if strings.TrimSpace(string(empty)) != `{"messages":[]}` {
		t.Fatalf("unexpected empty history: %s", empty)
	}

	call("POST", route, outsider, `{"nonce":"second","content":"Hi"}`, 404)
	call("POST", route, member, `{"nonce":"second","content":"Hi","author_id":"2"}`, 400)
	payload := `{"nonce":"second","content":"Hi"}`
	created := call("POST", route, member, payload, 200)
	repeated := call("POST", route, member, payload, 200)
	if string(created) != string(repeated) {
		t.Fatal("retry did not return the original message")
	}
	var message messages.Message
	if err := json.Unmarshal(created, &message); err != nil || message.AuthorID != 1 || message.ID <= firstID {
		t.Fatalf("unexpected sent message: %s, error %v", created, err)
	}

	var latest, older messages.HistoryPage
	page := call("GET", route+"?limit=1", member, "", 200)
	if err := json.Unmarshal(page, &latest); err != nil || len(latest.Messages) != 1 ||
		latest.Messages[0].ID != message.ID || latest.NextBefore != message.ID {
		t.Fatalf("unexpected first page: %s, error %v", page, err)
	}
	page = call("GET", fmt.Sprintf("%s?limit=1&before=%d", route, latest.NextBefore), member, "", 200)
	if err := json.Unmarshal(page, &older); err != nil || len(older.Messages) != 1 ||
		older.Messages[0].ID != firstID || older.NextBefore != 0 {
		t.Fatalf("unexpected second page: %s, error %v", page, err)
	}
}

func execSQL(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string) {
	t.Helper()
	if _, err := pool.Exec(ctx, query); err != nil {
		t.Fatal(err)
	}
}
