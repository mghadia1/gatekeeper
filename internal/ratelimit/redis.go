package ratelimit

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

// takeScript runs the whole token-bucket step inside Redis, so that concurrent
// requests through different gateway replicas cannot both spend the last
// token. It reads the time from Redis, not from the replicas, whose clocks
// may differ.
//
// Lua numbers returned to Redis are truncated to integers, so the
// fractional token count is stored as a string and only whole numbers are
// returned.
var takeScript = redis.NewScript(`
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local ttl_ms = tonumber(ARGV[3])

local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)

local state = redis.call('HMGET', key, 'tokens', 'ts')
local tokens = tonumber(state[1])
local ts = tonumber(state[2])
if tokens == nil or ts == nil then
  tokens = burst
  ts = now
end

tokens = math.min(burst, tokens + math.max(0, now - ts) * rate / 1000)
local allowed = 0
local retry_ms = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
else
  retry_ms = math.ceil((1 - tokens) * 1000 / rate)
end

redis.call('HSET', key, 'tokens', tostring(tokens), 'ts', tostring(now))
redis.call('PEXPIRE', key, ttl_ms)
return {allowed, math.floor(tokens), retry_ms}
`)

// RedisStore keeps buckets in Redis, shared by every gateway replica.
type RedisStore struct {
	client *redis.Client
	limit  Limit
	prefix string
	ttl    time.Duration
}

// NewRedisStore returns a store using client. Bucket keys expire once a
// bucket would have refilled completely: at that point it is the same as a
// new bucket, so Redis holds only clients that are active.
func NewRedisStore(client *redis.Client, limit Limit) *RedisStore {
	full := time.Duration(math.Ceil(float64(limit.Burst) / limit.Rate * float64(time.Second)))
	return &RedisStore{client: client, limit: limit, prefix: "gatekeeper:rl:", ttl: full + time.Second}
}

// Take spends one token from key's bucket if it has one.
func (s *RedisStore) Take(ctx context.Context, key string) (Decision, error) {
	result, err := takeScript.Run(ctx, s.client, []string{s.prefix + key},
		s.limit.Rate, s.limit.Burst, s.ttl.Milliseconds()).Int64Slice()
	if err != nil {
		return Decision{}, fmt.Errorf("rate limit store: %w", err)
	}
	if len(result) != 3 {
		return Decision{}, fmt.Errorf("rate limit store: unexpected reply %v", result)
	}
	return Decision{
		Allowed:    result[0] == 1,
		Remaining:  int(result[1]),
		RetryAfter: time.Duration(result[2]) * time.Millisecond,
	}, nil
}
