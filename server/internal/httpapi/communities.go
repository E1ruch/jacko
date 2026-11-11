package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"jacko/server/internal/communities"
	"jacko/server/internal/telemetry"
)

type communityAPI struct {
	service  *communities.Service
	presence MemberPresence
}

func RegisterCommunities(mux *http.ServeMux, service *communities.Service, auth Authenticator, presence MemberPresence, metrics *telemetry.Metrics) {
	api := communityAPI{service: service, presence: presence}
	handle := func(pattern, route string, handler func(http.ResponseWriter, *http.Request, int64)) {
		mux.Handle(pattern, metrics.HTTP(route, withUser(auth, handler)))
	}
	handle("POST /api/v1/guilds", "guild_create", api.createGuild)
	handle("GET /api/v1/guilds", "guild_list", api.listGuilds)
	handle("POST /api/v1/guilds/{guild}/channels", "channel_create", api.createChannel)
	handle("GET /api/v1/guilds/{guild}/channels", "channel_list", api.listChannels)
	handle("GET /api/v1/guilds/{guild}/members", "member_list", api.listMembers)
	handle("POST /api/v1/guilds/{guild}/invites", "invite_create", api.createInvite)
	handle("POST /api/v1/invites/redeem", "invite_redeem", api.redeemInvite)
}

type namedResource struct {
	Name string `json:"name"`
}

type resourcePage[T any] struct {
	Items     []T   `json:"items"`
	NextAfter int64 `json:"next_after,string,omitempty"`
}

func (a communityAPI) createGuild(w http.ResponseWriter, r *http.Request, userID int64) {
	var body namedResource
	if !readJSON(w, r, 4096, &body) {
		return
	}
	result, err := a.service.CreateGuild(r.Context(), userID, body.Name)
	writeCommunityResult(w, http.StatusCreated, result, err)
}

func (a communityAPI) listGuilds(w http.ResponseWriter, r *http.Request, userID int64) {
	after, ok := afterCursor(w, r)
	if !ok {
		return
	}
	guilds, err := a.service.ListGuilds(r.Context(), userID, after)
	page := resourcePage[communities.Guild]{Items: guilds}
	if len(guilds) > communities.PageSize {
		page.Items = guilds[:communities.PageSize]
		page.NextAfter = page.Items[communities.PageSize-1].ID
	}
	writeCommunityResult(w, http.StatusOK, page, err)
}

func (a communityAPI) createChannel(w http.ResponseWriter, r *http.Request, userID int64) {
	guildID, ok := guildParameter(w, r)
	if !ok {
		return
	}
	var body namedResource
	if !readJSON(w, r, 4096, &body) {
		return
	}
	result, err := a.service.CreateChannel(r.Context(), userID, guildID, body.Name)
	writeCommunityResult(w, http.StatusCreated, result, err)
}

func (a communityAPI) listChannels(w http.ResponseWriter, r *http.Request, userID int64) {
	guildID, ok := guildParameter(w, r)
	if !ok {
		return
	}
	after, ok := afterCursor(w, r)
	if !ok {
		return
	}
	channels, err := a.service.ListChannels(r.Context(), userID, guildID, after)
	page := resourcePage[communities.Channel]{Items: channels}
	if len(channels) > communities.PageSize {
		page.Items = channels[:communities.PageSize]
		page.NextAfter = page.Items[communities.PageSize-1].ID
	}
	writeCommunityResult(w, http.StatusOK, page, err)
}

func (a communityAPI) createInvite(w http.ResponseWriter, r *http.Request, userID int64) {
	guildID, ok := guildParameter(w, r)
	if !ok {
		return
	}
	invite, err := a.service.CreateInvite(r.Context(), userID, guildID)
	writeCommunityResult(w, http.StatusCreated, invite, err)
}

func (a communityAPI) redeemInvite(w http.ResponseWriter, r *http.Request, userID int64) {
	var body struct {
		Token string `json:"token"`
	}
	if !readJSON(w, r, 4096, &body) {
		return
	}
	guild, err := a.service.Join(r.Context(), userID, body.Token)
	writeCommunityResult(w, http.StatusOK, guild, err)
}

func guildParameter(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("guild"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_guild")
		return 0, false
	}
	return id, true
}

func afterCursor(w http.ResponseWriter, r *http.Request) (int64, bool) {
	query := r.URL.Query()
	if !query.Has("after") {
		return 0, true
	}
	id, err := strconv.ParseInt(query.Get("after"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_cursor")
		return 0, false
	}
	return id, true
}

func writeCommunityResult(w http.ResponseWriter, status int, result any, err error) {
	switch {
	case err == nil:
		writeJSON(w, status, result)
	case errors.Is(err, communities.ErrInvalidName):
		writeError(w, http.StatusBadRequest, "invalid_name")
	case errors.Is(err, communities.ErrUnavailable):
		writeError(w, http.StatusNotFound, "resource_not_found")
	default:
		internalError(w, err)
	}
}
