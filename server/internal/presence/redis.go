// Package presence tracks independent WebSocket connections with expiring leases.
package presence

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const lease = 50 * time.Second

var refreshLease = redis.NewScript(`
	local now = redis.call("TIME")
	local millis = now[1] * 1000 + math.floor(now[2] / 1000)
	redis.call("ZREMRANGEBYSCORE", KEYS[1], "-inf", millis)
	redis.call("ZADD", KEYS[1], millis + tonumber(ARGV[2]), ARGV[1])
	redis.call("PEXPIRE", KEYS[1], ARGV[2])
	return 1
`)

type Redis struct {
	client *redis.Client
	prefix string
}

func NewRedis(client *redis.Client, topic string) *Redis {
	if topic == "" {
		topic = "jacko:v1:messages"
	}
	return &Redis{client: client, prefix: topic + ":presence:"}
}

func (r *Redis) key(userID int64) string {
	return r.prefix + strconv.FormatInt(userID, 10)
}

func (r *Redis) Refresh(ctx context.Context, userID int64, connectionID string) error {
	return refreshLease.Run(ctx, r.client, []string{r.key(userID)}, connectionID, lease.Milliseconds()).Err()
}

func (r *Redis) Disconnect(ctx context.Context, userID int64, connectionID string) error {
	return r.client.ZRem(ctx, r.key(userID), connectionID).Err()
}

func (r *Redis) Online(ctx context.Context, userIDs []int64) (map[int64]bool, error) {
	online := make(map[int64]bool, len(userIDs))
	now, err := r.client.Time(ctx).Result()
	if err != nil {
		return nil, err
	}
	if len(userIDs) == 0 {
		return online, nil
	}
	cutoff := "(" + strconv.FormatInt(now.UnixMilli(), 10)
	pipe := r.client.Pipeline()
	counts := make([]*redis.IntCmd, len(userIDs))
	for i, userID := range userIDs {
		counts[i] = pipe.ZCount(ctx, r.key(userID), cutoff, "+inf")
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	for i, count := range counts {
		online[userIDs[i]] = count.Val() > 0
	}
	return online, nil
}
