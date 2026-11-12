package httpapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/netip"
	"time"

	"github.com/redis/go-redis/v9"
)

type RateLimiter interface {
	Allow(ctx context.Context, scope string, peer netip.Addr, burst int, interval time.Duration) (bool, error)
}

type RedisLimits struct {
	client *redis.Client
	prefix string
}

func NewRedisLimits(client *redis.Client, topic string) *RedisLimits {
	if topic == "" {
		topic = "jacko:v1:messages"
	}
	return &RedisLimits{client: client, prefix: topic + ":limit:"}
}

// The theoretical arrival time yields a shared token bucket using Redis time.
var rateScript = redis.NewScript(`
	local now = redis.call("TIME")
	local ms = now[1] * 1000 + math.floor(now[2] / 1000)
	local interval = tonumber(ARGV[1])
	local window = interval * tonumber(ARGV[2])
	local next = math.max(tonumber(redis.call("GET", KEYS[1])) or ms, ms) + interval
	if next - ms > window then return 0 end
	redis.call("SET", KEYS[1], string.format("%.0f", next), "PX", next - ms)
	return 1
`)

func (r *RedisLimits) Allow(ctx context.Context, scope string, peer netip.Addr, burst int, interval time.Duration) (bool, error) {
	key := fmt.Sprintf("%s%s:%x", r.prefix, scope, sha256.Sum256([]byte(peer.String())))
	allowed, err := rateScript.Run(ctx, r.client, []string{key}, interval.Milliseconds(), burst).Int()
	return allowed == 1, err
}
