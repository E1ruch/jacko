package httpapi

import (
	"context"
	"net/http"
	"time"

	"jacko/server/internal/communities"
)

type MemberPresence interface {
	Online(ctx context.Context, userIDs []int64) (map[int64]bool, error)
}

type memberStatus struct {
	communities.Member
	Online *bool `json:"online,omitempty"`
}

type memberPage struct {
	resourcePage[memberStatus]
	PresenceAvailable bool `json:"presence_available"`
}

func (a communityAPI) listMembers(w http.ResponseWriter, r *http.Request, userID int64) {
	guildID, ok := guildParameter(w, r)
	if !ok {
		return
	}
	after, ok := afterCursor(w, r)
	if !ok {
		return
	}

	members, err := a.service.ListMembers(r.Context(), userID, guildID, after)
	if err != nil {
		writeCommunityResult(w, http.StatusOK, nil, err)
		return
	}

	var page memberPage
	if len(members) > communities.PageSize {
		members = members[:communities.PageSize]
		page.NextAfter = members[len(members)-1].UserID
	}
	var online map[int64]bool
	if a.presence != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
		defer cancel()

		ids := make([]int64, 0, len(members))
		for _, member := range members {
			ids = append(ids, member.UserID)
		}
		statuses, err := a.presence.Online(ctx, ids)
		if err == nil {
			online = statuses
			page.PresenceAvailable = true
		}
	}
	page.Items = make([]memberStatus, 0, len(members))
	for _, member := range members {
		item := memberStatus{Member: member}
		if page.PresenceAvailable {
			status := online[member.UserID]
			item.Online = &status
		}
		page.Items = append(page.Items, item)
	}
	writeJSON(w, http.StatusOK, page)
}
