package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/jackc/pgx/v5/pgxpool"

	"jacko/server/internal/messages"
	"jacko/server/internal/sessions"
)

func testLifecycle(t *testing.T, ctx context.Context, pool *pgxpool.Pool, servers [2]*httptest.Server,
	auth *sessions.Service, ownerToken string, otherChannelMessage int64) {
	t.Helper()
	execSQL(t, ctx, pool, "INSERT INTO members (guild_id, user_id) VALUES (1, 2)")
	peer, err := auth.Issue(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	var channelID int64
	if err := pool.QueryRow(ctx, "INSERT INTO channels (guild_id, name) VALUES (1, 'lifecycle') RETURNING id").Scan(&channelID); err != nil {
		t.Fatal(err)
	}

	channelRoute := fmt.Sprintf("/api/v1/channels/%d", channelID)
	route := channelRoute + "/messages"
	call := func(method, path, token, body string, status int) []byte {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, method, servers[0].URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := servers[0].Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status {
			t.Fatalf("%s %s: status=%d, expected=%d, response=%s, error=%v", method, path, response.StatusCode, status, data, err)
		}
		return data
	}
	message := func(data []byte) messages.Message {
		t.Helper()
		var message messages.Message
		if err := json.Unmarshal(data, &message); err != nil || message.ID <= 0 {
			t.Fatalf("invalid message: %s, %v", data, err)
		}
		return message
	}

	live := dialChannel(t, ctx, servers[1], channelRoute, peer.AccessToken, "")
	defer live.CloseNow()
	readReady(t, ctx, live, 0)

	const rootRequest = `{"nonce":"root","content":"first draft"}`
	root := message(call("POST", route, ownerToken, rootRequest, 200))
	readEvent(t, ctx, live, 1, "first draft")
	ownerRoute := route + "/" + strconv.FormatInt(root.ID, 10)
	replyRequest := func(id int64) string {
		return fmt.Sprintf(`{"nonce":"reply","content":"reply","reply_to":"%d"}`, id)
	}
	call("POST", route, peer.AccessToken, replyRequest(otherChannelMessage), 404)
	reply := message(call("POST", route, peer.AccessToken, replyRequest(root.ID), 200))
	if reply.ReplyTo == nil || *reply.ReplyTo != root.ID {
		t.Fatalf("reply target not preserved: %+v", reply)
	}
	readEvent(t, ctx, live, 2, "reply")

	call("PATCH", ownerRoute, peer.AccessToken, `{"content":"wrong author"}`, 404)
	call("DELETE", ownerRoute, peer.AccessToken, "", 404)
	for i, text := range []string{"second draft", "final draft"} {
		body := fmt.Sprintf(`{"content":%q}`, text)
		edit := message(call("PATCH", ownerRoute, ownerToken, body, 200))
		if edit.Content != text || edit.EditedAt == nil {
			t.Fatalf("invalid edit: %+v", edit)
		}
		readMutation(t, ctx, live, int64(i)+3, "MESSAGE_UPDATE", root.ID, text)
		retry := message(call("PATCH", ownerRoute, ownerToken, body, 200))
		if retry.EditedAt == nil || !retry.EditedAt.Equal(*edit.EditedAt) {
			t.Fatal("unchanged edit advanced the timestamp")
		}
	}
	retry := message(call("POST", route, ownerToken, rootRequest, 200))
	if retry.ID != root.ID || retry.Content != "final draft" {
		t.Fatal("retry of original send did not return the edited message")
	}
	call("POST", route, ownerToken, `{"nonce":"root","content":"final draft"}`, 409)

	replayedEdits := dialChannel(t, ctx, servers[0], channelRoute, peer.AccessToken, "2")
	defer replayedEdits.CloseNow()
	readMutation(t, ctx, replayedEdits, 3, "MESSAGE_UPDATE", root.ID, "second draft")
	readMutation(t, ctx, replayedEdits, 4, "MESSAGE_UPDATE", root.ID, "final draft")
	readReady(t, ctx, replayedEdits, 4)
	_ = replayedEdits.CloseNow()

	call("DELETE", ownerRoute, ownerToken, "", 204)
	readMutation(t, ctx, live, 5, "MESSAGE_DELETE", root.ID, "")
	call("DELETE", ownerRoute, ownerToken, "", 204)
	call("PATCH", ownerRoute, ownerToken, `{"content":"restore"}`, 404)
	call("POST", route, ownerToken, rootRequest, 404)
	call("POST", route, peer.AccessToken, fmt.Sprintf(`{"nonce":"new-reply","content":"reply","reply_to":"%d"}`, root.ID), 404)
	if again := message(call("POST", route, peer.AccessToken, replyRequest(root.ID), 200)); again.ID != reply.ID {
		t.Fatal("retry after deleting the reply target created another message")
	}
	var history messages.HistoryPage
	data := call("GET", route, peer.AccessToken, "", 200)
	if err := json.Unmarshal(data, &history); err != nil || len(history.Messages) != 1 ||
		history.Messages[0].ID != reply.ID || history.Messages[0].ReplyTo == nil || *history.Messages[0].ReplyTo != root.ID {
		t.Fatalf("unexpected history after deletion: %s, %v", data, err)
	}

	resumed := dialChannel(t, ctx, servers[0], channelRoute, peer.AccessToken, "0")
	defer resumed.CloseNow()
	readMutation(t, ctx, resumed, 1, "MESSAGE_DELETE", root.ID, "")
	readEvent(t, ctx, resumed, 2, "reply")
	for seq := int64(3); seq <= 5; seq++ {
		readMutation(t, ctx, resumed, seq, "MESSAGE_DELETE", root.ID, "")
	}
	readReady(t, ctx, resumed, 5)
}

func readMutation(t *testing.T, ctx context.Context, conn *websocket.Conn, seq int64, eventType string, id int64, content string) {
	t.Helper()
	var frame struct {
		Seq  int64           `json:"seq,string"`
		Type string          `json:"t"`
		Data json.RawMessage `json:"d"`
	}
	if err := wsjson.Read(ctx, conn, &frame); err != nil || frame.Seq != seq || frame.Type != eventType {
		t.Fatalf("unexpected mutation frame: %+v, %v", frame, err)
	}
	if eventType == "MESSAGE_DELETE" {
		var tombstone messages.DeletedMessage
		if err := json.Unmarshal(frame.Data, &tombstone); err != nil || tombstone.ID != id ||
			tombstone.DeletedAt.IsZero() || strings.Contains(string(frame.Data), `"content"`) {
			t.Fatalf("invalid tombstone: %s, %v", frame.Data, err)
		}
		return
	}
	var updated messages.Message
	if err := json.Unmarshal(frame.Data, &updated); err != nil || updated.ID != id ||
		updated.EditedAt == nil || updated.Content != content {
		t.Fatalf("invalid update: %s, %v", frame.Data, err)
	}
}
