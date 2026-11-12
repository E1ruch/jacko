package postgres_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"jacko/server/internal/accounts"
	accountpg "jacko/server/internal/accounts/postgres"
	"jacko/server/internal/communities"
	"jacko/server/internal/httpapi"
	"jacko/server/internal/messages"
	"jacko/server/internal/moderation"
	"jacko/server/internal/sessions"
	sessionpg "jacko/server/internal/sessions/postgres"
)

func TestPasswordResetRevokesSessions(t *testing.T) {
	a := newResourceAPI(t)
	store := accountpg.NewStore(a.pool)
	auth := sessions.NewService(sessionpg.NewVerifiedStore(a.pool))
	const email = "verified@example.test"
	user, err := store.Create(a.ctx, email, accounts.Credential{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.IssueForPassword(a.ctx, user, 1); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("unverified login returned %v", err)
	}
	verify := [32]byte{1}
	reset := [32]byte{2}
	save := func(purpose accounts.Purpose, hash [32]byte) {
		t.Helper()
		if ok, err := store.SaveCode(a.ctx, email, purpose, hash); err != nil || !ok {
			t.Fatalf("save code: %v, %v", ok, err)
		}
	}
	save(accounts.VerifyEmail, verify)
	if err := store.ConsumeCode(a.ctx, accounts.ResetPassword, verify, accounts.Credential{}); !errors.Is(err, accounts.ErrInvalidCode) {
		t.Fatalf("verification code used for reset: %v", err)
	}
	if err := store.ConsumeCode(a.ctx, accounts.VerifyEmail, verify, accounts.Credential{}); err != nil {
		t.Fatal(err)
	}
	issued, err := auth.IssueForPassword(a.ctx, user, 1)
	if err != nil {
		t.Fatal(err)
	}

	save(accounts.ResetPassword, reset)
	var group sync.WaitGroup
	var results [2]error
	for i := range results {
		group.Go(func() {
			results[i] = store.ConsumeCode(a.ctx, accounts.ResetPassword, reset, accounts.Credential{})
		})
	}
	group.Wait()
	successes := 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case !errors.Is(err, accounts.ErrInvalidCode):
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("reset succeeded %d times", successes)
	}
	if _, err := auth.Authenticate(a.ctx, issued.AccessToken); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("old access token returned %v", err)
	}
	if _, err := auth.Refresh(a.ctx, issued.RefreshToken); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("old refresh token returned %v", err)
	}
	if _, err := auth.IssueForPassword(a.ctx, user, 1); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("stale password version issued a session: %v", err)
	}
	if _, err := auth.IssueForPassword(a.ctx, user, 2); err != nil {
		t.Fatal(err)
	}
}

func TestModerationAPI(t *testing.T) {
	a := newResourceAPI(t)
	httpapi.RegisterModeration(a.mux, moderation.NewStore(a.pool), a.auth, a.metrics, nil)
	guild := a.guild(t)
	owner, peer, outsider := a.owner.AccessToken, a.peer.AccessToken, a.outsider.AccessToken
	base := fmt.Sprintf("/api/v1/guilds/%d", guild.Guild.ID)
	invite := decodeResource[communities.Invite](t, a.call(t, "POST", base+"/invites", owner, "", 201))
	a.call(t, "POST", "/api/v1/invites/redeem", peer, fmt.Sprintf(`{"token":%q}`, invite.Token), 200)
	channel := fmt.Sprintf("/api/v1/channels/%d/messages", guild.Channel.ID)
	message := decodeResource[messages.Message](t, a.call(t, "POST", channel, owner, `{"nonce":"report","content":"message"}`, 200))
	report := fmt.Sprintf("%s/%d/reports", channel, message.ID)
	a.call(t, "POST", report, outsider, `{"reason":"denied"}`, 404)
	a.call(t, "POST", report, peer, `{"reason":"first report"}`, 204)
	a.call(t, "POST", report, peer, `{"reason":"duplicate"}`, 204)
	list := base + "/reports"
	a.call(t, "GET", list, peer, "", 404)
	type reportPage struct {
		Items []moderation.Report `json:"items"`
	}
	page := decodeResource[reportPage](t, a.call(t, "GET", list, owner, "", 200))
	if len(page.Items) != 1 || page.Items[0].Reason != "first report" || page.Items[0].Resolution != nil {
		t.Fatalf("unexpected report list: %+v", page)
	}
	resolve := fmt.Sprintf("%s/%d", list, page.Items[0].ID)
	a.call(t, "PATCH", resolve, peer, `{"resolution":"dismissed"}`, 404)
	a.call(t, "PATCH", resolve, owner, `{"resolution":"dismissed"}`, 204)
	page = decodeResource[reportPage](t, a.call(t, "GET", list, owner, "", 200))
	if len(page.Items) != 1 || page.Items[0].Resolution == nil || *page.Items[0].Resolution != "dismissed" {
		t.Fatalf("unexpected resolution: %+v", page)
	}
}
