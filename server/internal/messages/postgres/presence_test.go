package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"jacko/server/internal/communities"
	"jacko/server/internal/httpapi"
	"jacko/server/internal/sessions"
	"jacko/server/internal/telemetry"
)

type presencePage struct {
	Items []struct {
		UserID int64  `json:"user_id,string"`
		Role   string `json:"role"`
		Online *bool  `json:"online"`
	} `json:"items"`
	NextAfter         int64 `json:"next_after,string"`
	PresenceAvailable bool  `json:"presence_available"`
}

func testPresence(t *testing.T, ctx context.Context, pool *pgxpool.Pool, client *redis.Client, topic string,
	servers [2]*httptest.Server, auth *sessions.Service, ownerToken string, service *communities.Service) {
	t.Helper()
	guild, err := service.CreateGuild(ctx, 1, "presence")
	if err != nil {
		t.Fatal(err)
	}
	invite, err := service.CreateInvite(ctx, 1, guild.Guild.ID)
	if err != nil {
		t.Fatal(err)
	}
	var peerID int64
	if err := pool.QueryRow(ctx, "INSERT INTO users DEFAULT VALUES RETURNING id").Scan(&peerID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Join(ctx, peerID, invite.Token); err != nil {
		t.Fatal(err)
	}
	devices := make([]sessions.Tokens, 2)
	for i := range devices {
		devices[i], err = auth.Issue(ctx, peerID)
		if err != nil {
			t.Fatal(err)
		}
	}
	var outsiderID int64
	if err := pool.QueryRow(ctx, "INSERT INTO users DEFAULT VALUES RETURNING id").Scan(&outsiderID); err != nil {
		t.Fatal(err)
	}
	outsider, err := auth.Issue(ctx, outsiderID)
	if err != nil {
		t.Fatal(err)
	}

	route := fmt.Sprintf("/api/v1/guilds/%d/members", guild.Guild.ID)
	readPage := func(handler http.Handler, path, token string, want int) presencePage {
		t.Helper()
		request := httptest.NewRequest("GET", path, nil).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("members status %d, want %d: %s", response.Code, want, response.Body.String())
		}
		var page presencePage
		if want == http.StatusOK {
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
		}
		return page
	}

	handler := servers[0].Config.Handler
	readPage(handler, route, outsider.AccessToken, 404)
	readPage(handler, route+"?after=invalid", ownerToken, 400)
	initial := readPage(handler, route, ownerToken, 200)
	if len(initial.Items) != 2 || initial.Items[0].UserID != 1 || initial.Items[0].Role != "owner" ||
		initial.Items[1].UserID != peerID || initial.Items[1].Role != "member" || !initial.PresenceAvailable {
		t.Fatalf("invalid members: %+v", initial)
	}
	offline := initial.Items[1].Online
	if offline == nil || *offline {
		t.Fatal("member without a socket should be offline")
	}

	channelRoute := fmt.Sprintf("/api/v1/channels/%d", guild.Channel.ID)
	first := dialChannel(t, ctx, servers[0], channelRoute, devices[0].AccessToken, "")
	defer first.CloseNow()
	readReady(t, ctx, first, 0)
	second := dialChannel(t, ctx, servers[1], channelRoute, devices[1].AccessToken, "")
	defer second.CloseNow()
	readReady(t, ctx, second, 0)

	checkOnline := func() bool {
		t.Helper()
		page := readPage(handler, route+"?after=1", ownerToken, 200)
		if len(page.Items) != 1 || page.Items[0].UserID != peerID || page.Items[0].Online == nil {
			t.Fatalf("invalid member cursor page: %+v", page)
		}
		return *page.Items[0].Online
	}
	key := fmt.Sprintf("%s:presence:%d", topic, peerID)
	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil || ttl <= 0 || ttl > 50*time.Second || !checkOnline() {
		t.Fatalf("unexpected lease TTL or online state: %s, %v", ttl, err)
	}

	// An expired lease must not keep the user online while the key still exists.
	now, err := client.Time(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ZAdd(ctx, key, redis.Z{Score: float64(now.UnixMilli() - 1), Member: "expired-connection"}).Err(); err != nil {
		t.Fatal(err)
	}
	waitForLeases := func(want int64) {
		t.Helper()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			count, err := client.ZCard(ctx, key).Result()
			if err != nil {
				t.Fatal(err)
			}
			if count == want {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("lease count = %d, want %d", count, want)
			case <-ticker.C:
			}
		}
	}
	_ = first.CloseNow()
	waitForLeases(2)
	if !checkOnline() {
		t.Fatal("closing the first device marked the second device offline")
	}
	_ = second.CloseNow()
	waitForLeases(1)
	if checkOnline() {
		t.Fatal("expired lease kept user online")
	}

	fallback := http.NewServeMux()
	httpapi.RegisterCommunities(fallback, service, auth, unavailablePresence{}, telemetry.New())
	degraded := readPage(fallback, route, ownerToken, 200)
	if degraded.PresenceAvailable || len(degraded.Items) != 2 || degraded.Items[0].Online != nil ||
		degraded.Items[1].Online != nil {
		t.Fatalf("invalid degraded members response: %+v", degraded)
	}

	_, err = pool.Exec(ctx, `
		WITH generated AS (
			INSERT INTO users (id) OVERRIDING SYSTEM VALUE
			SELECT nextval(pg_get_serial_sequence('users', 'id')) FROM generate_series(1, 100)
			RETURNING id
		)
		INSERT INTO members (guild_id, user_id) SELECT $1, id FROM generated
	`, guild.Guild.ID)
	if err != nil {
		t.Fatal(err)
	}
	page := readPage(handler, route, ownerToken, 200)
	if len(page.Items) != communities.PageSize || page.NextAfter != page.Items[len(page.Items)-1].UserID {
		t.Fatal("full page did not return its last user as next_after")
	}
	tail := readPage(handler, fmt.Sprintf("%s?after=%d", route, page.NextAfter), ownerToken, 200)
	if len(tail.Items) != 2 || tail.Items[0].UserID <= page.NextAfter || tail.NextAfter != 0 {
		t.Fatalf("invalid final members page: %+v", tail)
	}
}

type unavailablePresence struct{}

func (unavailablePresence) Online(context.Context, []int64) (map[int64]bool, error) {
	return nil, context.DeadlineExceeded
}
