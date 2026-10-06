package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mghadia1/gatekeeper/internal/config"
	"github.com/mghadia1/gatekeeper/internal/ratelimit"
)

// limiter applies a rate limit to the routes it wraps. Only proxied routes
// are limited: /healthz and 404s cost nothing to answer.
type limiter struct {
	store     ratelimit.Store
	burst     int
	keyHeader string
	failOpen  bool
	logger    *slog.Logger
	lastError atomic.Int64 // unix nanoseconds of the last store error logged
	downUntil atomic.Int64 // unix nanoseconds until which the store is skipped
	closer    func() error
}

const (
	// storeErrorLogEvery keeps a store outage from writing one log line per
	// request.
	storeErrorLogEvery = 5 * time.Second
	// storeBackoff: after a store error, skip the store this long and apply
	// the failure policy at once. An outage then costs nothing per request,
	// and a recovering Redis is not hit by every request at once.
	storeBackoff = time.Second
)

var errStoreBackingOff = errors.New("rate limit store failed recently; not retrying yet")

// redisLog sends go-redis's own messages (pool and dial errors) into the
// gateway's JSON log instead of letting it print plain text to stderr.
type redisLog struct{ logger *slog.Logger }

func (l redisLog) Printf(ctx context.Context, format string, v ...any) {
	l.logger.DebugContext(ctx, fmt.Sprintf(format, v...), "component", "go-redis")
}

var setRedisLogger sync.Once

func newLimiter(cfg *config.RateLimit, logger *slog.Logger) (*limiter, error) {
	limit := ratelimit.Limit{Rate: cfg.RequestsPerSecond, Burst: cfg.Burst}
	l := &limiter{burst: cfg.Burst, keyHeader: cfg.KeyHeader, failOpen: cfg.FailOpen, logger: logger,
		closer: func() error { return nil }}
	switch cfg.Store {
	case "redis":
		opts, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return nil, fmt.Errorf("rate_limit.redis_url: %w", err)
		}
		// The limiter sits in front of every request: if Redis is slow, give
		// up quickly and apply the store-error policy rather than queue.
		opts.DialTimeout = 500 * time.Millisecond
		opts.ReadTimeout = 200 * time.Millisecond
		opts.WriteTimeout = 200 * time.Millisecond
		// No retries: by default go-redis retries a failed command 3 times
		// and a failed dial 5 times with backoff, which turned "Redis is
		// down, fail open" into 1.7 s of extra latency on every request.
		opts.MaxRetries = -1 // -1, not 0: 0 means "use the default"
		opts.DialerRetries = 1
		opts.DialerRetryTimeout = time.Millisecond
		setRedisLogger.Do(func() { redis.SetLogger(redisLog{logger}) })
		client := redis.NewClient(opts)
		l.store = ratelimit.NewRedisStore(client, limit)
		l.closer = client.Close
	default:
		l.store = ratelimit.NewMemoryStore(limit, nil)
	}
	return l, nil
}

func (l *limiter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := ratelimit.ClientKey(r, l.keyHeader)
		info := infoFrom(r.Context())
		info.client = key

		decision, err := l.take(r.Context(), key)
		if err != nil {
			l.logStoreError(err)
			if l.failOpen {
				next.ServeHTTP(w, r)
				return
			}
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error":      "rate limiter unavailable",
				"request_id": requestID(r.Context()),
			})
			return
		}

		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(l.burst))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(decision.Remaining))
		if !decision.Allowed {
			info.rateLimited = true
			// Retry-After is in whole seconds; round up, so a client that
			// obeys it is never early.
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(decision.RetryAfter.Seconds()))))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
				"error":          "rate limit exceeded",
				"retry_after_ms": decision.RetryAfter.Milliseconds(),
				"request_id":     requestID(r.Context()),
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// take asks the store, unless it failed less than storeBackoff ago.
func (l *limiter) take(ctx context.Context, key string) (ratelimit.Decision, error) {
	if time.Now().UnixNano() < l.downUntil.Load() {
		return ratelimit.Decision{}, errStoreBackingOff
	}
	decision, err := l.store.Take(ctx, key)
	if err != nil {
		l.downUntil.Store(time.Now().Add(storeBackoff).UnixNano())
	}
	return decision, err
}

func (l *limiter) logStoreError(err error) {
	now := time.Now().UnixNano()
	last := l.lastError.Load()
	if now-last < int64(storeErrorLogEvery) || !l.lastError.CompareAndSwap(last, now) {
		return
	}
	policy := "letting requests through"
	if !l.failOpen {
		policy = "refusing requests with 503"
	}
	l.logger.Warn("rate limit store unavailable; "+policy, "error", err.Error())
}
