package resilience

import (
	"sync"
	"time"
)

// Breaker is a circuit breaker for one backend.
//
// Closed, it lets everything through and watches the last ten seconds. Once
// there have been at least MinRequests and the share that failed reaches
// FailureRatio, it opens: requests are refused at once, rather than each
// waiting out a timeout against a backend that is not answering. After
// OpenFor it lets a single probe through (half-open): if the probe succeeds
// it closes, if it fails it opens again.
//
// This is a backend-wide decision. Ejecting one bad instance is the
// balancer's job; the breaker is for when the whole backend is failing.
type Breaker struct {
	FailureRatio float64
	MinRequests  int
	OpenFor      time.Duration
	Now          func() time.Time // nil means time.Now

	mu       sync.Mutex
	state    State
	openedAt time.Time
	probing  bool
	recent   window // a: requests, b: failures
}

// State is the breaker's position.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	return [...]string{"closed", "open", "half-open"}[s]
}

// Outcome is how a request through the breaker ended.
type Outcome int

const (
	Success Outcome = iota
	Failure
	// Ignored is for requests that tell nothing about the backend, such as
	// a client that went away; a probe that ends this way frees the slot
	// for another.
	Ignored
)

func (b *Breaker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Allow reports whether a request may go to the backend. If not, retryAfter
// is how long until the breaker will try again. Every allowed request must
// be followed by exactly one Record.
func (b *Breaker) Allow() (allowed bool, retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	switch b.state {
	case Open:
		if wait := b.openedAt.Add(b.OpenFor).Sub(now); wait > 0 {
			return false, wait
		}
		b.state = HalfOpen
		b.probing = false
		fallthrough
	case HalfOpen:
		if b.probing {
			return false, b.OpenFor
		}
		b.probing = true
		return true, 0
	default:
		return true, 0
	}
}

// Record reports how an allowed request ended.
func (b *Breaker) Record(outcome Outcome) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if b.state == HalfOpen {
		b.probing = false
		switch outcome {
		case Success:
			b.state = Closed
			b.recent.reset()
		case Failure:
			b.state = Open
			b.openedAt = now
		}
		return
	}
	if b.state != Closed || outcome == Ignored {
		return
	}
	failed := 0
	if outcome == Failure {
		failed = 1
	}
	b.recent.add(now, 1, failed)
	requests, failures := b.recent.totals(now)
	if requests >= b.MinRequests && float64(failures) >= b.FailureRatio*float64(requests) {
		b.state = Open
		b.openedAt = now
		b.recent.reset()
	}
}

// State returns the breaker's position, for logging and tests.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && !b.now().Before(b.openedAt.Add(b.OpenFor)) {
		return HalfOpen // ready to probe, though nothing has asked yet
	}
	return b.state
}
