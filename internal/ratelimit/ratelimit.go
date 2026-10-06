// Package ratelimit limits how fast each client may send requests, with a
// token bucket per client.
//
// A bucket holds up to Burst tokens and refills at Rate tokens per second; a
// request spends one token or is refused. Tokens are fractional: a bucket
// that refills 10 per second has half a token 50 ms after it emptied, and a
// whole one at 100 ms. (Counting whole tokens only and resetting the clock on
// every request would make a client who asks exactly at the refill rate get
// almost nothing through.)
//
// Two stores implement it: MemoryStore, for a single gateway, and
// RedisStore, which several gateway replicas share so that a client's limit
// is one limit and not one per replica.
package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net"
	"net/http"
	"time"
)

// Limit is a token bucket's shape.
type Limit struct {
	Rate  float64 // tokens added per second
	Burst int     // most tokens the bucket holds
}

// Decision is the outcome of one request.
type Decision struct {
	Allowed bool
	// Remaining is how many whole tokens are left after this request.
	Remaining int
	// RetryAfter, when refused, is how long until a token is available.
	RetryAfter time.Duration
}

// Store takes a token from key's bucket.
type Store interface {
	Take(ctx context.Context, key string) (Decision, error)
}

// refill is the token-bucket arithmetic both stores use: the bucket's tokens
// after elapsed time, then the decision for one request.
func refill(limit Limit, tokens float64, elapsed time.Duration) (float64, Decision) {
	tokens = math.Min(float64(limit.Burst), tokens+elapsed.Seconds()*limit.Rate)
	if tokens >= 1 {
		tokens--
		return tokens, Decision{Allowed: true, Remaining: int(tokens)}
	}
	wait := time.Duration(math.Ceil((1 - tokens) / limit.Rate * float64(time.Second)))
	return tokens, Decision{Allowed: false, Remaining: 0, RetryAfter: wait}
}

// ClientKey identifies who is asking: the API key in header, if the request
// has one, otherwise the IP address the connection comes from.
//
// API keys are hashed, so they never appear in Redis or the logs. The
// connection's address is used rather than X-Forwarded-For, which any client
// can set to anything; behind a load balancer every client would then share
// that load balancer's address, which is a configuration to add when there
// is one, not a header to trust by default.
func ClientKey(r *http.Request, header string) string {
	if header != "" {
		if key := r.Header.Get(header); key != "" {
			sum := sha256.Sum256([]byte(key))
			return "key:" + hex.EncodeToString(sum[:8])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "ip:" + host
}
