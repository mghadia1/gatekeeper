package balancer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func urls(t *testing.T, raw ...string) []*url.URL {
	t.Helper()
	out := make([]*url.URL, len(raw))
	for i, r := range raw {
		u, err := url.Parse(r)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = u
	}
	return out
}

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

func picks(p *Pool, n int) map[string]int {
	counts := map[string]int{}
	for range n {
		counts[p.Pick().URL.Host]++
	}
	return counts
}

func TestRoundRobinIsEven(t *testing.T) {
	p := New("b", urls(t, "http://a", "http://b", "http://c"), Options{})
	counts := picks(p, 300)
	for _, host := range []string{"a", "b", "c"} {
		if counts[host] != 100 {
			t.Fatalf("picks = %v, want 100 each", counts)
		}
	}
}

func TestEjectedInstanceIsSkippedThenReturns(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	p := New("b", urls(t, "http://a", "http://b"), Options{EjectAfter: 2, EjectFor: 10 * time.Second, Now: c.now})
	bad := p.Instances()[1]

	p.ReportFailure(bad)
	if got := picks(p, 10); got["b"] != 5 {
		t.Fatalf("after 1 failure (below EjectAfter) picks = %v, want b still in rotation", got)
	}
	p.ReportFailure(bad)
	if got := picks(p, 10); got["b"] != 0 || got["a"] != 10 {
		t.Fatalf("after 2 failures picks = %v, want only a", got)
	}
	c.advance(10 * time.Second)
	if got := picks(p, 10); got["b"] != 5 {
		t.Fatalf("after the ejection period picks = %v, want b back", got)
	}
}

func TestSuccessResetsTheFailureStreak(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	p := New("b", urls(t, "http://a", "http://b"), Options{EjectAfter: 2, Now: c.now})
	bad := p.Instances()[1]
	p.ReportFailure(bad)
	p.ReportSuccess(bad)
	p.ReportFailure(bad)
	if !bad.Healthy(c.now()) {
		t.Fatal("two failures separated by a success ejected the instance")
	}
}

func TestFailsOpenWhenNoInstanceIsHealthy(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	p := New("b", urls(t, "http://a", "http://b"), Options{EjectAfter: 1, Now: c.now})
	for _, inst := range p.Instances() {
		p.ReportFailure(inst)
	}
	if got := picks(p, 10); got["a"] != 5 || got["b"] != 5 {
		t.Fatalf("with every instance ejected picks = %v, want all of them in turn", got)
	}
}

func TestPickIsSafeUnderConcurrency(t *testing.T) {
	p := New("b", urls(t, "http://a", "http://b", "http://c", "http://d"), Options{EjectAfter: 1000})
	var counts [4]atomic.Int64
	index := map[string]int{"a": 0, "b": 1, "c": 2, "d": 3}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				inst := p.Pick()
				counts[index[inst.URL.Host]].Add(1)
				p.ReportFailure(inst)
				p.ReportSuccess(inst)
			}
		}()
	}
	wg.Wait()
	for i := range counts {
		if got := counts[i].Load(); got != 2000 {
			t.Fatalf("instance %d picked %d times, want 2000 (8000 picks over 4)", i, got)
		}
	}
}

// flaky serves /healthz with whatever status it is set to.
type flaky struct {
	status atomic.Int32
	*httptest.Server
}

func newFlaky(t *testing.T) *flaky {
	f := &flaky{}
	f.status.Store(http.StatusOK)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(f.status.Load()))
	}))
	t.Cleanup(f.Close)
	return f
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestActiveChecksMarkDownAndUpWithThresholds(t *testing.T) {
	good, bad := newFlaky(t), newFlaky(t)
	hc := &HealthCheck{Path: "/healthz", Interval: 10 * time.Millisecond, Timeout: time.Second, UnhealthyAfter: 3, HealthyAfter: 2}
	p := New("b", urls(t, good.URL, bad.URL), Options{HealthCheck: hc})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	badInst := p.Instances()[1]
	bad.status.Store(http.StatusInternalServerError)
	waitFor(t, "the failing instance to be marked down", func() bool { return !badInst.Healthy(time.Now()) })
	if got := picks(p, 10); got[badInst.URL.Host] != 0 {
		t.Fatalf("picks = %v, want none to the unhealthy instance", got)
	}

	bad.status.Store(http.StatusOK)
	waitFor(t, "the recovered instance to be marked up", func() bool { return badInst.Healthy(time.Now()) })
}

func TestOneFailedCheckDoesNotEject(t *testing.T) {
	bad := newFlaky(t)
	hc := &HealthCheck{Path: "/healthz", Interval: time.Hour, Timeout: time.Second, UnhealthyAfter: 2, HealthyAfter: 1}
	p := New("b", urls(t, bad.URL), Options{HealthCheck: hc})
	inst := p.Instances()[0]
	bad.status.Store(http.StatusServiceUnavailable)

	p.check(context.Background(), inst)
	if !inst.Healthy(time.Now()) {
		t.Fatal("one failed check marked the instance down; UnhealthyAfter is 2")
	}
	p.check(context.Background(), inst)
	if inst.Healthy(time.Now()) {
		t.Fatal("two failed checks did not mark the instance down")
	}
}

func TestAPassingCheckEndsAnEjection(t *testing.T) {
	good := newFlaky(t)
	hc := &HealthCheck{Path: "/healthz", Interval: time.Hour, Timeout: time.Second, UnhealthyAfter: 1, HealthyAfter: 1}
	p := New("b", urls(t, good.URL), Options{HealthCheck: hc, EjectAfter: 1, EjectFor: time.Hour})
	inst := p.Instances()[0]

	// Mark it down by checks, eject it by a request failure, then let a
	// check pass: it should be back without waiting out the hour.
	good.status.Store(http.StatusInternalServerError)
	p.check(context.Background(), inst)
	p.ReportFailure(inst)
	good.status.Store(http.StatusOK)
	p.check(context.Background(), inst)
	if !inst.Healthy(time.Now()) {
		t.Fatal("a passing health check did not end the ejection")
	}
}

func TestRunWithoutChecksReturnsImmediately(t *testing.T) {
	p := New("b", urls(t, "http://a"), Options{})
	done := make(chan struct{})
	go func() { p.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run without health checks did not return")
	}
}
