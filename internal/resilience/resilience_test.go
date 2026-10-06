package resilience

import (
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newBreaker(c *clock) *Breaker {
	return &Breaker{FailureRatio: 0.5, MinRequests: 10, OpenFor: 5 * time.Second, Now: c.now}
}

func through(b *Breaker, outcome Outcome) bool {
	ok, _ := b.Allow()
	if ok {
		b.Record(outcome)
	}
	return ok
}

func TestBreakerStaysClosedBelowMinimumRequests(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	b := newBreaker(c)
	for range 9 {
		through(b, Failure)
	}
	if b.State() != Closed {
		t.Fatalf("9 failures (below MinRequests 10) opened the breaker: %v", b.State())
	}
}

func TestBreakerOpensAtTheFailureRatio(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	b := newBreaker(c)
	for i := range 10 {
		outcome := Success
		if i%2 == 0 {
			outcome = Failure
		}
		through(b, outcome)
	}
	if b.State() != Open {
		t.Fatalf("5 failures of 10 at ratio 0.5: state %v, want open", b.State())
	}
	ok, retryAfter := b.Allow()
	if ok || retryAfter != 5*time.Second {
		t.Fatalf("open breaker Allow = (%v, %v), want (false, 5s)", ok, retryAfter)
	}
}

func TestBreakerIgnoresOldFailures(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	b := newBreaker(c)
	for range 8 {
		through(b, Failure)
	}
	c.advance(11 * time.Second) // those failures leave the ten-second window
	for range 8 {
		through(b, Success)
	}
	through(b, Failure)
	through(b, Failure)
	if b.State() != Closed {
		t.Fatalf("2 recent failures of 10 opened the breaker: %v", b.State())
	}
}

func TestHalfOpenLetsOneProbeThroughAndCloses(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	b := newBreaker(c)
	for range 10 {
		through(b, Failure)
	}
	c.advance(5 * time.Second)

	ok, _ := b.Allow()
	if !ok {
		t.Fatal("after OpenFor the breaker did not let a probe through")
	}
	if second, _ := b.Allow(); second {
		t.Fatal("a second request got through while the probe was in flight")
	}
	b.Record(Success)
	if b.State() != Closed {
		t.Fatalf("after a successful probe: %v, want closed", b.State())
	}
	if !through(b, Success) {
		t.Fatal("the closed breaker refused a request")
	}
}

func TestAFailedProbeReopens(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	b := newBreaker(c)
	for range 10 {
		through(b, Failure)
	}
	c.advance(5 * time.Second)
	through(b, Failure) // the probe
	if ok, wait := b.Allow(); ok || wait != 5*time.Second {
		t.Fatalf("after a failed probe Allow = (%v, %v), want open again for 5s", ok, wait)
	}
}

func TestAnIgnoredProbeFreesTheSlot(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	b := newBreaker(c)
	for range 10 {
		through(b, Failure)
	}
	c.advance(5 * time.Second)
	b.Allow()
	b.Record(Ignored) // the client went away mid-probe
	if ok, _ := b.Allow(); !ok {
		t.Fatal("after an ignored probe, no new probe was allowed")
	}
}

func TestBreakerIsSafeUnderConcurrency(t *testing.T) {
	b := &Breaker{FailureRatio: 0.5, MinRequests: 100, OpenFor: time.Second}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				outcome := Success
				if i%3 == 0 {
					outcome = Failure
				}
				through(b, outcome)
			}
		}()
	}
	wg.Wait()
	if b.State() != Closed {
		t.Fatalf("a third failing (below 0.5) opened the breaker: %v", b.State())
	}
}

func TestRetryBudgetAllowsTheMinimumWhenQuiet(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := &RetryBudget{Ratio: 0.2, MinPerSecond: 1, Now: c.now}
	allowed := 0
	for range 20 {
		if r.TryRetry() {
			allowed++
		}
	}
	if allowed != 10 { // MinPerSecond 1 over the 10 s window
		t.Fatalf("%d retries with no traffic, want 10 (1/s over 10 s)", allowed)
	}
}

func TestRetryBudgetGrowsWithTraffic(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	r := &RetryBudget{Ratio: 0.2, MinPerSecond: 0, Now: c.now}
	for range 100 {
		r.Request()
	}
	allowed := 0
	for range 100 {
		if r.TryRetry() {
			allowed++
		}
	}
	if allowed != 20 {
		t.Fatalf("%d retries for 100 requests at ratio 0.2, want 20", allowed)
	}
	c.advance(11 * time.Second) // the window has moved on
	if r.TryRetry() {
		t.Fatal("a retry was allowed with no requests in the window and no minimum")
	}
}
