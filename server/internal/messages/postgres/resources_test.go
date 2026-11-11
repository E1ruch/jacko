package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jacko/server/internal/communities"
	communitypg "jacko/server/internal/communities/postgres"
	"jacko/server/internal/httpapi"
	"jacko/server/internal/messages"
	messagepg "jacko/server/internal/messages/postgres"
	"jacko/server/internal/sessions"
	sessionpg "jacko/server/internal/sessions/postgres"
	"jacko/server/internal/telemetry"
)

type resourceAPI struct {
	ctx                   context.Context
	pool                  *pgxpool.Pool
	mux                   *http.ServeMux
	auth                  *sessions.Service
	metrics               *telemetry.Metrics
	owner, peer, outsider sessions.Tokens
}

func newResourceAPI(t *testing.T) *resourceAPI {
	t.Helper()
	dsn := os.Getenv("JACKO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set JACKO_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("jacko_resources_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	execSQL(t, ctx, admin, "CREATE SCHEMA "+identifier)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		execSQL(t, cleanup, admin, "DROP SCHEMA "+identifier+" CASCADE")
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	migrations, err := filepath.Glob("../../../migrations/*.up.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatalf("find migrations: %v", err)
	}
	for _, migration := range migrations {
		applyMigration(t, ctx, pool, filepath.Base(migration))
	}
	api := &resourceAPI{ctx: ctx, pool: pool, mux: http.NewServeMux(),
		auth: sessions.NewService(sessionpg.NewStore(pool)), metrics: telemetry.New()}
	api.mux.Handle("/", httpapi.New(messages.NewService(messagepg.NewStore(pool)), api.auth, api.metrics))
	httpapi.RegisterCommunities(api.mux, communities.NewService(communitypg.NewStore(pool)), api.auth, nil, api.metrics)
	for _, tokens := range []*sessions.Tokens{&api.owner, &api.peer, &api.outsider} {
		var id int64
		if err := pool.QueryRow(ctx, "INSERT INTO users DEFAULT VALUES RETURNING id").Scan(&id); err != nil {
			t.Fatal(err)
		}
		*tokens, err = api.auth.Issue(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	return api
}

func (a *resourceAPI) call(t *testing.T, method, path, token, body string, want int) []byte {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(a.ctx)
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	a.mux.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("%s %s: got %d, want %d: %s", method, path, response.Code, want, response.Body)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("private response may be cached")
	}
	return response.Body.Bytes()
}

func decodeResource[T any](t *testing.T, data []byte) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func (a *resourceAPI) guild(t *testing.T) communities.CreatedGuild {
	t.Helper()
	return decodeResource[communities.CreatedGuild](t, a.call(t, "POST", "/api/v1/guilds", a.owner.AccessToken, `{"name":"Resources"}`, 201))
}

func TestCommunitiesAPI(t *testing.T) {
	a := newResourceAPI(t)
	owner, peer, outsider := a.owner.AccessToken, a.peer.AccessToken, a.outsider.AccessToken
	a.call(t, "GET", "/api/v1/guilds", "", "", 401)
	empty := a.call(t, "GET", "/api/v1/guilds", peer, "", 200)
	if strings.TrimSpace(string(empty)) != `{"items":[]}` {
		t.Fatalf("invalid empty page: %s", empty)
	}
	a.call(t, "POST", "/api/v1/guilds", owner, `{"name":"\u0000bad"}`, 400)
	guild := a.guild(t)
	if guild.Guild.OwnerID == nil || *guild.Guild.OwnerID != a.owner.UserID || guild.Channel.GuildID != guild.Guild.ID {
		t.Fatalf("invalid created guild: %+v", guild)
	}
	base := fmt.Sprintf("/api/v1/guilds/%d", guild.Guild.ID)
	for _, route := range []string{"/channels", "/members"} {
		a.call(t, "GET", base+route, outsider, "", 404)
		a.call(t, "GET", base+route+"?after=0", owner, "", 400)
	}
	a.call(t, "POST", base+"/channels", outsider, `{"name":"secret"}`, 404)
	a.call(t, "POST", base+"/invites", outsider, "", 404)
	invite := decodeResource[communities.Invite](t, a.call(t, "POST", base+"/invites", owner, "", 201))
	body := fmt.Sprintf(`{"token":%q}`, invite.Token)
	for range 2 {
		joined := decodeResource[communities.Guild](t, a.call(t, "POST", "/api/v1/invites/redeem", peer, body, 200))
		if joined.ID != guild.Guild.ID {
			t.Fatal("joined another guild")
		}
	}
	a.call(t, "POST", base+"/channels", peer, `{"name":"forbidden"}`, 404)
	a.call(t, "POST", base+"/invites", peer, "", 404)
	a.call(t, "POST", "/api/v1/invites/redeem", outsider, `{"token":"bad"}`, 404)
	channel := decodeResource[communities.Channel](t, a.call(t, "POST", base+"/channels", owner, `{"name":"second"}`, 201))
	if channel.GuildID != guild.Guild.ID {
		t.Fatal("channel created in another guild")
	}
	if _, err := a.pool.Exec(a.ctx, "UPDATE guild_invites SET expires_at = now() - interval '1 second'"); err != nil {
		t.Fatal(err)
	}
	a.call(t, "POST", "/api/v1/invites/redeem", outsider, body, 404)
	var members int
	if err := a.pool.QueryRow(a.ctx, "SELECT count(*) FROM members WHERE guild_id = $1", guild.Guild.ID).Scan(&members); err != nil || members != 2 {
		t.Fatalf("duplicate or missing membership: %d, %v", members, err)
	}
	if _, err := a.pool.Exec(a.ctx, `INSERT INTO channels (guild_id, name) SELECT $1, 'page-' || n FROM generate_series(1, 100) AS n`, guild.Guild.ID); err != nil {
		t.Fatal(err)
	}
	type page struct {
		Items []communities.Channel `json:"items"`
		Next  int64                 `json:"next_after,string"`
	}
	first := decodeResource[page](t, a.call(t, "GET", base+"/channels", peer, "", 200))
	if len(first.Items) != 100 || first.Next != first.Items[99].ID {
		t.Fatal("invalid page boundary")
	}
	tail := decodeResource[page](t, a.call(t, "GET", fmt.Sprintf("%s/channels?after=%d", base, first.Next), peer, "", 200))
	if len(tail.Items) != 2 || tail.Next != 0 || tail.Items[0].ID <= first.Next {
		t.Fatal("invalid continuation page")
	}
}

func TestRequestDeadlineCancelsDatabaseWait(t *testing.T) {
	a := newResourceAPI(t)
	guild := a.guild(t)
	tx, err := a.pool.Begin(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(a.ctx)
	if _, err := tx.Exec(a.ctx, "SELECT id FROM channels WHERE id = $1 FOR UPDATE", guild.Channel.ID); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/channels/%d/messages", guild.Channel.ID)
	body := `{"nonce":"blocked","content":"must not commit"}`
	ctx, cancel := context.WithTimeout(a.ctx, 100*time.Millisecond)
	defer cancel()
	request := httptest.NewRequest("POST", path, strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+a.owner.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.mux.ServeHTTP(response, request)
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("blocked query returned %d: %s", response.Code, response.Body)
	}
	if err := tx.Rollback(a.ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := a.pool.QueryRow(a.ctx, "SELECT count(*) FROM messages").Scan(&count); err != nil || count != 0 {
		t.Fatalf("timed out write committed: %d messages, %v", count, err)
	}
	// Retrying after the lock is released must succeed, with no stuck transaction.
	a.call(t, "POST", path, a.owner.AccessToken, body, 200)
}
