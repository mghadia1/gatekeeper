// Package balancer spreads a backend's requests across its instances and
// keeps track of which instances are healthy.
//
// Two signals decide health:
//
//   - Active checks (optional): a background goroutine per instance requests
//     a health path on an interval. An instance is marked down after
//     UnhealthyAfter consecutive failed checks and up again after
//     HealthyAfter consecutive successes, so one blip does not flip it.
//   - Passive detection (always on): real requests that fail at the
//     connection level, time out, or get a 503 count against the instance.
//     After EjectAfter consecutive failures it is ejected for EjectFor, then
//     tried again.
//
// An instance takes traffic only if both agree it is fine. If no instance
// does, Pick returns any instance anyway (fail open): a wrong health signal
// should degrade to "some requests fail", not "every request is refused".
package balancer

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// HealthCheck configures active checking. A nil *HealthCheck means none.
type HealthCheck struct {
	Path           string
	Interval       time.Duration
	Timeout        time.Duration
	UnhealthyAfter int
	HealthyAfter   int
}

// Options configures a Pool.
type Options struct {
	HealthCheck *HealthCheck
	EjectAfter  int           // consecutive request failures before ejection
	EjectFor    time.Duration // how long an ejected instance sits out
	Logger      *slog.Logger
	// Now is the clock for ejection; tests replace it. Defaults to time.Now.
	Now func() time.Time
}

// Instance is one address of a backend.
type Instance struct {
	URL *url.URL

	mu              sync.Mutex
	activeHealthy   bool // the active checker's verdict; true if there is none
	checkStreak     int  // consecutive checks agreeing against the verdict
	requestFailures int  // consecutive failed requests
	ejectedUntil    time.Time
}

// Pool is a backend's set of instances.
type Pool struct {
	name      string
	instances []*Instance
	opts      Options
	next      atomic.Uint64
	client    *http.Client
}

// New returns a pool over urls. Every instance starts healthy, so the gateway
// serves traffic immediately; checks correct that within a few intervals.
func New(name string, urls []*url.URL, opts Options) *Pool {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.EjectAfter < 1 {
		opts.EjectAfter = 3
	}
	if opts.EjectFor <= 0 {
		opts.EjectFor = 10 * time.Second
	}
	p := &Pool{name: name, opts: opts}
	for _, u := range urls {
		p.instances = append(p.instances, &Instance{URL: u, activeHealthy: true})
	}
	if opts.HealthCheck != nil {
		p.client = &http.Client{Timeout: opts.HealthCheck.Timeout}
	}
	return p
}

// Instances returns the pool's instances, in configuration order.
func (p *Pool) Instances() []*Instance { return p.instances }

// Pick returns the next instance in round-robin order among the healthy
// ones, or, if none is healthy, among all of them.
func (p *Pool) Pick() *Instance {
	now := p.opts.Now()
	start := p.next.Add(1) - 1
	n := uint64(len(p.instances))
	for i := range n {
		inst := p.instances[(start+i)%n]
		if inst.Healthy(now) {
			return inst
		}
	}
	return p.instances[start%n] // fail open
}

// PickExcept is Pick for a retry: the next healthy instance other than skip,
// or any other instance if none is healthy, or skip itself if it is the
// only one.
func (p *Pool) PickExcept(skip *Instance) *Instance {
	n := uint64(len(p.instances))
	if n == 1 {
		return p.instances[0]
	}
	now := p.opts.Now()
	start := p.next.Add(1) - 1
	var fallback *Instance
	for i := range n {
		inst := p.instances[(start+i)%n]
		if inst == skip {
			continue
		}
		if inst.Healthy(now) {
			return inst
		}
		if fallback == nil {
			fallback = inst
		}
	}
	return fallback
}

// Healthy reports whether the instance should take traffic at time now.
func (inst *Instance) Healthy(now time.Time) bool {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.activeHealthy && !now.Before(inst.ejectedUntil)
}

// ReportSuccess records a request the instance served.
func (p *Pool) ReportSuccess(inst *Instance) {
	inst.mu.Lock()
	inst.requestFailures = 0
	inst.mu.Unlock()
}

// ReportFailure records a request that failed because of the instance, and
// ejects it after EjectAfter in a row.
func (p *Pool) ReportFailure(inst *Instance) {
	inst.mu.Lock()
	inst.requestFailures++
	eject := inst.requestFailures >= p.opts.EjectAfter
	if eject {
		inst.requestFailures = 0
		inst.ejectedUntil = p.opts.Now().Add(p.opts.EjectFor)
	}
	inst.mu.Unlock()
	if eject {
		p.opts.Logger.Warn("instance ejected after failed requests",
			"backend", p.name, "instance", inst.URL.String(),
			"failures", p.opts.EjectAfter, "for", p.opts.EjectFor.String())
	}
}

// Run starts active health checks, if configured, and blocks until ctx is
// cancelled.
func (p *Pool) Run(ctx context.Context) {
	if p.opts.HealthCheck == nil {
		return
	}
	var wg sync.WaitGroup
	for _, inst := range p.instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(p.opts.HealthCheck.Interval)
			defer ticker.Stop()
			for {
				p.check(ctx, inst)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	wg.Wait()
}

// check runs one active check and moves the verdict once enough consecutive
// checks disagree with it.
func (p *Pool) check(ctx context.Context, inst *Instance) {
	hc := p.opts.HealthCheck
	ok := p.probe(ctx, inst)
	if ctx.Err() != nil {
		return // shutting down: not the instance's fault
	}

	inst.mu.Lock()
	changed := false
	if ok == inst.activeHealthy {
		inst.checkStreak = 0
	} else {
		inst.checkStreak++
		need := hc.UnhealthyAfter
		if ok {
			need = hc.HealthyAfter
		}
		if inst.checkStreak >= need {
			inst.activeHealthy = ok
			inst.checkStreak = 0
			if ok {
				inst.ejectedUntil = time.Time{} // a passing check ends an ejection
			}
			changed = true
		}
	}
	inst.mu.Unlock()

	if changed {
		level, msg := slog.LevelWarn, "instance marked unhealthy by health checks"
		if ok {
			level, msg = slog.LevelInfo, "instance marked healthy by health checks"
		}
		p.opts.Logger.Log(ctx, level, msg, "backend", p.name, "instance", inst.URL.String())
	}
}

func (p *Pool) probe(ctx context.Context, inst *Instance) bool {
	target := inst.URL.JoinPath(p.opts.HealthCheck.Path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
