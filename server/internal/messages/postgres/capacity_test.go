package postgres_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/redis/go-redis/v9"

	"jacko/server/internal/gateway"
	"jacko/server/internal/outbox"
	"jacko/server/internal/presence"
	"jacko/server/internal/realtime"
)

func TestGatewayCapacity(t *testing.T) {
	configured := os.Getenv("JACKO_TEST_WS_CAPACITY")
	if configured == "" {
		t.Skip("set JACKO_TEST_WS_CAPACITY to run the one-gateway capacity test")
	}
	count, err := strconv.Atoi(configured)
	if err != nil || count < 1 || count > 10_000 {
		t.Fatal("JACKO_TEST_WS_CAPACITY must be between 1 and 10000")
	}
	redisURL := os.Getenv("JACKO_TEST_REDIS_URL")
	if redisURL == "" {
		t.Fatal("set JACKO_TEST_REDIS_URL")
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	options.ContextTimeoutEnabled = true
	client := redis.NewClient(options)
	defer client.Close()

	a := newResourceAPI(t)
	guild := a.guild(t)
	route := fmt.Sprintf("/api/v1/channels/%d", guild.Channel.ID)
	ctx, cancel := context.WithCancel(a.ctx)
	hub := realtime.NewHub(a.metrics)
	topic := fmt.Sprintf("jacko:capacity:%d", time.Now().UnixNano())
	broker := realtime.NewRedis(client, topic)
	tracker := presence.NewRedis(client, topic)
	worker := outbox.NewWorker(a.pool, broker, a.metrics)
	var backend sync.WaitGroup
	defer func() { cancel(); backend.Wait() }()
	backend.Go(func() { broker.Run(ctx, hub) })
	backend.Go(func() { worker.Run(ctx) })
	waitForHub(t, ctx, hub, guild.Channel.ID)
	a.mux.Handle("GET /api/v1/channels/{channel}/events",
		gateway.New(ctx, hub, a.auth, outbox.NewJournal(a.pool), a.metrics, tracker))
	server := httptest.NewServer(a.mux)
	defer server.Close()

	// Independent identities exercise real membership and presence lookups.
	_, err = a.pool.Exec(ctx, `
		WITH users_added AS (
		    INSERT INTO users (id) OVERRIDING SYSTEM VALUE
		    SELECT nextval(pg_get_serial_sequence('users', 'id')) FROM generate_series(1, $2::int)
		    RETURNING id
		), memberships AS (
		    INSERT INTO members (guild_id, user_id) SELECT $1, id FROM users_added
		)
		INSERT INTO sessions (user_id, token_hash, expires_at)
		SELECT id, sha256(decode(lpad(to_hex(id), 64, '0'), 'hex')), now() + interval '15 minutes'
		FROM users_added
	`, guild.Guild.ID, count)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := a.pool.Query(ctx, "SELECT user_id FROM members WHERE guild_id = $1 AND user_id <> $2 ORDER BY user_id",
		guild.Guild.ID, a.owner.UserID)
	if err != nil {
		t.Fatal(err)
	}
	tokens := make([]string, 0, count)
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tokens = append(tokens, fmt.Sprintf("%064x", userID))
	}
	if err := rows.Err(); err != nil || len(tokens) != count {
		t.Fatalf("capacity identities: got %d, %v", len(tokens), err)
	}

	const messages = 5
	connections := make([]*websocket.Conn, count)
	samples := make([]time.Duration, count*messages)
	failures := make(chan error, count)
	var readers sync.WaitGroup
	var dialers sync.WaitGroup
	var submitted [messages]time.Time
	started := make([]chan struct{}, messages)
	for i := range started {
		started[i] = make(chan struct{})
	}
	defer func() {
		cancel()
		readers.Wait()
		for _, conn := range connections {
			if conn != nil {
				_ = conn.CloseNow()
			}
		}
	}()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + route + "/events"
	connectStart := time.Now()
	for workerID := range min(32, count) {
		dialers.Go(func() {
			for i := workerID; i < count && ctx.Err() == nil; i += min(32, count) {
				conn, _, err := websocket.Dial(ctx, wsURL, nil)
				if err != nil {
					failures <- fmt.Errorf("connect client %d: %w", i, err)
					cancel()
					return
				}
				connections[i] = conn
				var ready struct {
					Op string `json:"op"`
				}
				err = wsjson.Write(ctx, conn, map[string]string{"op": "identify", "token": tokens[i]})
				if err == nil {
					err = wsjson.Read(ctx, conn, &ready)
				}
				if err != nil || ready.Op != "ready" {
					failures <- fmt.Errorf("identify client %d: %s, %v", i, ready.Op, err)
					cancel()
					return
				}
				readers.Go(func() {
					for j := range messages {
						var frame receivedEvent
						err := wsjson.Read(ctx, conn, &frame)
						received := time.Now()
						if err != nil || frame.Seq != int64(j+1) || frame.Data.Content != strconv.Itoa(j) {
							failures <- fmt.Errorf("client %d expected frame %d: seq=%d, %v", i, j+1, frame.Seq, err)
							cancel()
							return
						}
						<-started[j]
						samples[i*messages+j] = received.Sub(submitted[j])
					}
				})
			}
		})
	}
	dialers.Wait()
	if ctx.Err() != nil {
		t.Fatal(<-failures)
	}
	t.Logf("opened %d sockets on one gateway in %s", count, time.Since(connectStart).Round(time.Millisecond))
	timer := time.NewTimer(21 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		t.Fatal(<-failures)
	case <-timer.C:
	}
	for i := range messages {
		submitted[i] = time.Now()
		close(started[i])
		if err := sendHTTP(ctx, server, route, a.owner.AccessToken, fmt.Sprintf("capacity-%d", i), strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	readers.Wait()
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
	slices.Sort(samples)
	t.Logf("one gateway after 21s hold: %d deliveries, p50=%s p95=%s p99=%s",
		len(samples), samples[(len(samples)*50+99)/100-1].Round(time.Millisecond),
		samples[(len(samples)*95+99)/100-1].Round(time.Millisecond), samples[(len(samples)*99+99)/100-1].Round(time.Millisecond))
}
