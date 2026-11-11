package postgres_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"jacko/server/internal/gateway"
	"jacko/server/internal/httpapi"
	"jacko/server/internal/messages"
	"jacko/server/internal/outbox"
	"jacko/server/internal/realtime"
	"jacko/server/internal/sessions"
	sessionpg "jacko/server/internal/sessions/postgres"
	"jacko/server/internal/telemetry"
)

func testRealtime(t *testing.T, parent context.Context, pool *pgxpool.Pool, service *messages.Service, schema string) {
	t.Helper()
	redisURL := os.Getenv("JACKO_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("set JACKO_TEST_REDIS_URL to run cross-gateway delivery test")
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	options.ContextTimeoutEnabled = true
	client := redis.NewClient(options)
	defer client.Close()

	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	broker := realtime.NewRedis(client, "jacko:test:"+schema)
	publisher := &retryPublisher{broker: broker}
	metrics := telemetry.New()
	worker := outbox.NewWorker(pool, publisher, metrics)
	workers.Go(func() { worker.ObserveBacklog(ctx) })
	waitForMetrics(t, ctx, metrics.Handler(), "jacko_outbox_probe_ok 1\n")
	workers.Go(func() { worker.Run(ctx) })

	var channelID int64
	err = pool.QueryRow(ctx, "INSERT INTO channels (guild_id, name) VALUES (1, 'realtime') RETURNING id").Scan(&channelID)
	if err != nil {
		t.Fatal(err)
	}
	var secret [32]byte
	secret[0] = 123
	hash := sha256.Sum256(secret[:])
	if _, err := pool.Exec(ctx, "INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, 1, now() + interval '1 hour')", hash[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(secret[:])

	auth := sessions.NewService(sessionpg.NewStore(pool))
	channelRoute := "/api/v1/channels/" + strconv.FormatInt(channelID, 10)
	var servers [2]*httptest.Server
	var connections [2]*websocket.Conn
	var hubs [2]*realtime.Hub
	for i := range servers {
		hub := realtime.NewHub(metrics)
		hubs[i] = hub
		workers.Go(func() { broker.Run(ctx, hub) })
		waitForHub(t, ctx, hub, channelID)

		mux := http.NewServeMux()
		mux.Handle("/", httpapi.New(service, auth, metrics))
		mux.Handle("GET /api/v1/channels/{channel}/events", gateway.New(ctx, hub, auth, outbox.NewJournal(pool), metrics))
		server := httptest.NewServer(mux)
		servers[i] = server
		defer server.Close()

		connections[i] = dialChannel(t, ctx, server, channelRoute, token, "")
		defer connections[i].CloseNow()
		readReady(t, ctx, connections[i], 0)
	}
	metrics.ObserveQueueDepth(func() float64 {
		return hubs[0].QueuedEvents() + hubs[1].QueuedEvents()
	})

	if err := sendHTTP(ctx, servers[0], channelRoute, token, "realtime", "retry-once"); err != nil {
		t.Fatal(err)
	}
	first := readEvent(t, ctx, connections[0], 1, "retry-once")
	second := readEvent(t, ctx, connections[1], 1, "retry-once")
	if first.ID <= 0 || first.ID != second.ID || first.Data.ChannelID != channelID || first.Data.AuthorID != 1 {
		t.Fatalf("unexpected cross-gateway events: %+v, %+v", first, second)
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var published bool
		if err := pool.QueryRow(ctx, "SELECT published_at IS NOT NULL FROM message_outbox WHERE id = $1", first.ID).Scan(&published); err != nil {
			t.Fatal(err)
		}
		if published {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("outbox event was not marked published:", ctx.Err())
		case <-ticker.C:
		}
	}
	if !publisher.failed.Load() {
		t.Fatal("expected one simulated publication failure")
	}

	if err := connections[1].CloseNow(); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"drop-notification", "while-offline"} {
		if err := sendHTTP(ctx, servers[0], channelRoute, token, content, content); err != nil {
			t.Fatal(err)
		}
	}

	missed := readEvent(t, ctx, connections[0], 2, "drop-notification")
	latest := readEvent(t, ctx, connections[0], 3, "while-offline")
	resumed := dialChannel(t, ctx, servers[1], channelRoute, token, "1")
	defer resumed.CloseNow()
	if readEvent(t, ctx, resumed, 2, "drop-notification").ID != missed.ID ||
		readEvent(t, ctx, resumed, 3, "while-offline").ID != latest.ID {
		t.Fatal("replayed events changed identity")
	}
	readReady(t, ctx, resumed, 3)

	if err := sendHTTP(ctx, servers[1], channelRoute, token, "after-resume", "after-resume"); err != nil {
		t.Fatal(err)
	}
	readEvent(t, ctx, resumed, 4, "after-resume")
	readEvent(t, ctx, connections[0], 4, "after-resume")

	invalidCursor := dialChannel(t, ctx, servers[1], channelRoute, token, "100")
	defer invalidCursor.CloseNow()
	var reset struct {
		Op string `json:"op"`
	}
	if err := wsjson.Read(ctx, invalidCursor, &reset); err != nil || reset.Op != "resync_required" {
		t.Fatalf("invalid cursor: %+v, %v", reset, err)
	}
	waitForMetrics(t, ctx, metrics.Handler(),
		`jacko_http_request_duration_seconds_count{code="200",route="message_send"} 4`,
		`jacko_ws_dispatch_total{mode="live"} 5`,
		`jacko_ws_dispatch_total{mode="replay"} 3`,
		"jacko_ws_connections 2\n",
		"jacko_ws_resync_required_total 1\n",
		"jacko_ws_live_delivery_seconds_count 5\n",
		"jacko_ws_queued_events 0\n",
		"jacko_outbox_publish_failures_total 1\n",
	)

	if os.Getenv("JACKO_TEST_LOAD") == "1" {
		_ = resumed.CloseNow()
		_ = connections[0].CloseNow()
		measureDelivery(t, ctx, servers, channelRoute, token, 4)
	}
	testLifecycle(t, ctx, pool, servers, auth, token, first.Data.ID)
}

func waitForMetrics(t *testing.T, ctx context.Context, handler http.Handler, expected ...string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		scrape := httptest.NewRecorder()
		handler.ServeHTTP(scrape, httptest.NewRequest("GET", "/metrics", nil).WithContext(ctx))
		if scrape.Code != http.StatusOK {
			t.Fatalf("metrics scrape returned %d", scrape.Code)
		}
		body := scrape.Body.String()
		var missing []string
		for _, sample := range expected {
			if !strings.Contains(body, sample) {
				missing = append(missing, sample)
			}
		}
		if len(missing) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("missing metrics samples %q: %v", missing, ctx.Err())
		case <-ticker.C:
		}
	}
}

type receivedEvent struct {
	ID   int64            `json:"event_id,string"`
	Seq  int64            `json:"seq,string"`
	Type string           `json:"t"`
	Data messages.Message `json:"d"`
}

func readEvent(t *testing.T, ctx context.Context, conn *websocket.Conn, seq int64, content string) receivedEvent {
	t.Helper()
	var event receivedEvent
	if err := wsjson.Read(ctx, conn, &event); err != nil || event.Seq != seq ||
		event.Type != "MESSAGE_CREATE" || event.Data.Content != content {
		t.Fatalf("expected sequence %d and content %q, got %+v, %v", seq, content, event, err)
	}
	return event
}

func dialChannel(t *testing.T, ctx context.Context, server *httptest.Server, route, token, after string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + route + "/events"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	identify := map[string]string{"op": "identify", "token": token}
	if after != "" {
		identify["after"] = after
	}
	if err := wsjson.Write(ctx, conn, identify); err != nil {
		_ = conn.CloseNow()
		t.Fatal(err)
	}
	return conn
}

func readReady(t *testing.T, ctx context.Context, conn *websocket.Conn, cursor int64) {
	t.Helper()
	var frame struct {
		Op     string `json:"op"`
		Cursor int64  `json:"cursor,string"`
	}
	if err := wsjson.Read(ctx, conn, &frame); err != nil || frame.Op != "ready" || frame.Cursor != cursor {
		t.Fatalf("expected ready at %d, got %+v, %v", cursor, frame, err)
	}
}

func sendHTTP(ctx context.Context, server *httptest.Server, route, token, nonce, content string) error {
	body, err := json.Marshal(map[string]string{"nonce": nonce, "content": content})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, "POST", server.URL+route+"/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("send returned %d", response.StatusCode)
	}
	_, err = io.Copy(io.Discard, response.Body)
	return err
}

type retryPublisher struct {
	broker *realtime.Redis
	failed atomic.Bool
}

func (p *retryPublisher) Publish(ctx context.Context, payload []byte) error {
	if strings.Contains(string(payload), "retry-once") && p.failed.CompareAndSwap(false, true) {
		return errors.New("simulated publication failure")
	}
	if strings.Contains(string(payload), "drop-notification") {
		return nil
	}
	return p.broker.Publish(ctx, payload)
}

func waitForHub(t *testing.T, ctx context.Context, hub *realtime.Hub, channelID int64) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, unsubscribe, ok := hub.Subscribe(channelID); ok {
			unsubscribe()
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("gateway did not subscribe to Redis:", ctx.Err())
		case <-ticker.C:
		}
	}
}
