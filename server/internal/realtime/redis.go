package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

type Redis struct {
	client *redis.Client
	topic  string
}

func NewRedis(client *redis.Client, topic string) *Redis {
	if topic == "" {
		topic = "jacko:v1:messages"
	}
	return &Redis{client: client, topic: topic}
}

func (r *Redis) Publish(ctx context.Context, payload []byte) error {
	return r.client.Publish(ctx, r.topic, payload).Err()
}

func (r *Redis) Run(ctx context.Context, hub *Hub) {
	for ctx.Err() == nil {
		err := r.subscribe(ctx, hub)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("realtime subscription interrupted", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (r *Redis) subscribe(ctx context.Context, hub *Hub) error {
	ready, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	sub := r.client.Subscribe(ready, r.topic)
	defer sub.Close()
	if confirmation, err := sub.ReceiveTimeout(ready, 3*time.Second); err != nil {
		return err
	} else if _, ok := confirmation.(*redis.Subscription); !ok {
		return errors.New("expected Redis subscription acknowledgement")
	}

	events := sub.ChannelWithSubscriptions(redis.WithChannelHealthCheckInterval(5 * time.Second))
	hub.Connect()
	defer hub.Disconnect()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				return errors.New("Redis subscription closed")
			}
			switch value := event.(type) {
			case *redis.Subscription:
				// A new acknowledgement means go-redis reconnected. Some events may be missing.
				hub.Disconnect()
				hub.Connect()
			case *redis.Message:
				receivedAt := time.Now()
				var route struct {
					Seq  int64 `json:"seq,string"`
					Data struct {
						ChannelID int64 `json:"channel_id,string"`
					} `json:"d"`
				}
				if err := json.Unmarshal([]byte(value.Payload), &route); err != nil || route.Seq <= 0 || route.Data.ChannelID <= 0 {
					slog.Warn("ignoring malformed realtime event")
					continue
				}
				hub.Deliver(route.Data.ChannelID, Frame{Seq: route.Seq, Payload: []byte(value.Payload), ReceivedAt: receivedAt})
			}
		}
	}
}
