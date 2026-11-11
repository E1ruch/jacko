// Package gateway serves authenticated, read-only WebSocket subscriptions.
package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"jacko/server/internal/messages"
	"jacko/server/internal/realtime"
	"jacko/server/internal/sessions"
	"jacko/server/internal/telemetry"
)

const (
	maxReplayEvents  = 256
	operationTimeout = 5 * time.Second
)

var errResync = errors.New("channel cursor requires a fresh history snapshot")

type Authenticator interface {
	Authenticate(ctx context.Context, token string) (int64, error)
}

type ChannelJournal interface {
	Head(ctx context.Context, userID, channelID int64) (int64, error)
	Read(ctx context.Context, channelID, after, through int64) ([]realtime.Event, error)
}

type Gateway struct {
	lifetime context.Context
	hub      *realtime.Hub
	auth     Authenticator
	journal  ChannelJournal
	metrics  *telemetry.Metrics
}

func New(lifetime context.Context, hub *realtime.Hub, auth Authenticator, journal ChannelJournal, metrics *telemetry.Metrics) *Gateway {
	return &Gateway{lifetime: lifetime, hub: hub, auth: auth, journal: journal, metrics: metrics}
}

type identity struct {
	Op    string `json:"op"`
	Token string `json:"token"`
	After *int64 `json:"after,string"`
}

type cursorFrame struct {
	Version int    `json:"v"`
	Op      string `json:"op"`
	Cursor  int64  `json:"cursor,string"`
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	channelID, err := strconv.ParseInt(r.PathValue("channel"), 10, 64)
	if err != nil || channelID <= 0 {
		http.Error(w, "invalid channel", http.StatusBadRequest)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1024)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(g.lifetime, cancel)
	defer stop()

	request, userID, err := g.identify(ctx, conn)
	if err != nil {
		g.closeForError(ctx, conn, err)
		return
	}

	subscription, unsubscribe, ok := g.hub.Subscribe(channelID)
	if !ok {
		_ = conn.Close(websocket.StatusTryAgainLater, "realtime unavailable")
		return
	}
	defer unsubscribe()

	ctx = conn.CloseRead(ctx)
	cursor, err := g.prepare(ctx, conn, userID, channelID, request.After)
	if err != nil {
		g.closeForError(ctx, conn, err)
		return
	}

	g.metrics.WSConnections.Inc()
	defer g.metrics.WSConnections.Dec()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		// Prioritize broker disconnection and backpressure over queued frames.
		select {
		case <-ctx.Done():
			return
		case <-subscription.Done:
			_ = conn.Close(websocket.StatusTryAgainLater, "reconnect with cursor")
			return
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-subscription.Done:
			_ = conn.Close(websocket.StatusTryAgainLater, "reconnect with cursor")
			return
		case <-heartbeat.C:
			cursor, err = g.checkConnection(ctx, conn, request.Token, userID, channelID, cursor)
		case frame := <-subscription.Events:
			if frame.Seq <= cursor {
				continue
			}
			cursor, err = g.deliver(ctx, conn, channelID, cursor, frame)
		}
		if err != nil {
			g.closeForError(ctx, conn, err)
			return
		}
	}
}

func (g *Gateway) identify(ctx context.Context, conn *websocket.Conn) (identity, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()

	var request identity
	if err := wsjson.Read(ctx, conn, &request); err != nil {
		return identity{}, 0, sessions.ErrInvalid
	}
	if request.Op != "identify" || (request.After != nil && *request.After < 0) {
		return identity{}, 0, sessions.ErrInvalid
	}
	userID, err := g.auth.Authenticate(ctx, request.Token)
	return request, userID, err
}

func (g *Gateway) prepare(ctx context.Context, conn *websocket.Conn, userID, channelID int64, after *int64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()

	// The live queue is already registered. Everything committed before this
	// snapshot can be replayed, and any overlap with queued events is skipped.
	head, err := g.journal.Head(ctx, userID, channelID)
	if err != nil {
		return 0, err
	}
	if after != nil {
		if err := g.replay(ctx, conn, channelID, *after, head); err != nil {
			return 0, err
		}
	}
	return head, wsjson.Write(ctx, conn, cursorFrame{Version: 1, Op: "ready", Cursor: head})
}

func (g *Gateway) deliver(ctx context.Context, conn *websocket.Conn, channelID, cursor int64, frame realtime.Frame) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()

	if frame.Seq != cursor+1 {
		if err := g.replay(ctx, conn, channelID, cursor, frame.Seq-1); err != nil {
			return cursor, err
		}
	}
	if err := conn.Write(ctx, websocket.MessageText, frame.Payload); err != nil {
		return cursor, err
	}
	g.metrics.WSFrames.WithLabelValues("live").Inc()
	g.metrics.WSLiveDelivery.Observe(time.Since(frame.ReceivedAt).Seconds())
	return frame.Seq, nil
}

func (g *Gateway) replay(ctx context.Context, conn *websocket.Conn, channelID, after, through int64) error {
	if after > through || through-after > maxReplayEvents {
		return errResync
	}
	for after < through {
		events, err := g.journal.Read(ctx, channelID, after, through)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return errResync
		}
		for _, event := range events {
			if event.Seq != after+1 {
				return errResync
			}
			if err := wsjson.Write(ctx, conn, event); err != nil {
				return err
			}
			g.metrics.WSFrames.WithLabelValues("replay").Inc()
			after = event.Seq
		}
	}
	return nil
}

func (g *Gateway) checkConnection(ctx context.Context, conn *websocket.Conn, token string, userID, channelID, cursor int64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()

	currentUser, err := g.auth.Authenticate(ctx, token)
	if err != nil {
		return cursor, err
	}
	if currentUser != userID {
		return cursor, sessions.ErrInvalid
	}
	head, err := g.journal.Head(ctx, userID, channelID)
	if err != nil {
		return cursor, err
	}
	if err := g.replay(ctx, conn, channelID, cursor, head); err != nil {
		return cursor, err
	}
	return head, conn.Ping(ctx)
}

func (g *Gateway) closeForError(ctx context.Context, conn *websocket.Conn, err error) {
	if ctx.Err() != nil {
		return
	}
	switch {
	case errors.Is(err, sessions.ErrInvalid), errors.Is(err, messages.ErrChannelAccess):
		_ = conn.Close(websocket.StatusPolicyViolation, "subscription denied")
	case errors.Is(err, errResync):
		g.metrics.WSResyncs.Inc()
		ctx, cancel := context.WithTimeout(ctx, operationTimeout)
		defer cancel()
		if wsjson.Write(ctx, conn, struct {
			Version int    `json:"v"`
			Op      string `json:"op"`
		}{Version: 1, Op: "resync_required"}) == nil {
			_ = conn.Close(websocket.StatusPolicyViolation, "history snapshot required")
		}
	default:
		slog.Warn("websocket connection interrupted", "error", err)
		_ = conn.Close(websocket.StatusTryAgainLater, "connection interrupted")
	}
}
