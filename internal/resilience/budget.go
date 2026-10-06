package resilience

import (
	"sync"
	"time"
)

// RetryBudget caps retries as a share of requests.
//
// Without a cap, retries multiply load exactly when a backend is struggling:
// if every request fails once and is retried, the backend sees twice the
// traffic it could not handle in the first place (a retry storm). The budget
// allows a retry only while retries over the last ten seconds stay under
// Ratio times the requests in that time, plus MinPerSecond so that a quiet
// backend can still retry at all.
type RetryBudget struct {
	Ratio        float64
	MinPerSecond float64
	Now          func() time.Time // nil means time.Now

	mu     sync.Mutex
	recent window // a: requests, b: retries
}

func (r *RetryBudget) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Request records a request (not counting its retries).
func (r *RetryBudget) Request() {
	r.mu.Lock()
	r.recent.add(r.now(), 1, 0)
	r.mu.Unlock()
}

// TryRetry reports whether a retry fits in the budget, and if so counts it.
func (r *RetryBudget) TryRetry() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	requests, retries := r.recent.totals(now)
	allowance := r.MinPerSecond*windowBuckets + r.Ratio*float64(requests)
	if float64(retries)+1 > allowance {
		return false
	}
	r.recent.add(now, 0, 1)
	return true
}
