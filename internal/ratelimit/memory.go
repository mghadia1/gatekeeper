package ratelimit

import (
	"context"
	"sync"
	"time"
)

// MemoryStore keeps buckets in this process. Each gateway replica has its own,
// so with N replicas a client can get N times its limit; use RedisStore there.
type MemoryStore struct {
	limit Limit
	now   func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// sweepEvery is how often idle buckets are dropped. A bucket that has been
// idle long enough to refill completely is the same as no bucket, so it can
// go; otherwise every client that ever sent one request would cost memory
// forever.
const sweepEvery = time.Minute

// NewMemoryStore returns an empty store. now is the clock; nil means
// time.Now.
func NewMemoryStore(limit Limit, now func() time.Time) *MemoryStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryStore{limit: limit, now: now, buckets: map[string]*bucket{}, lastSweep: now()}
}

// Take spends one token from key's bucket if it has one.
func (m *MemoryStore) Take(_ context.Context, key string) (Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if now.Sub(m.lastSweep) >= sweepEvery {
		m.sweep(now)
	}
	b, ok := m.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(m.limit.Burst), last: now}
		m.buckets[key] = b
	}
	tokens, decision := refill(m.limit, b.tokens, now.Sub(b.last))
	b.tokens, b.last = tokens, now
	return decision, nil
}

func (m *MemoryStore) sweep(now time.Time) {
	full := time.Duration(float64(m.limit.Burst) / m.limit.Rate * float64(time.Second))
	for key, b := range m.buckets {
		if now.Sub(b.last) >= full {
			delete(m.buckets, key)
		}
	}
	m.lastSweep = now
}

// Len is how many buckets the store holds.
func (m *MemoryStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.buckets)
}
