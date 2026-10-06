package gateway

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mghadia1/gatekeeper/internal/balancer"
	"github.com/mghadia1/gatekeeper/internal/resilience"
)

// unmatched labels requests no route matched (404s, 405s, /healthz), so the
// route label only ever holds configured patterns.
const unmatched = "unmatched"

// metrics are the gateway's Prometheus metrics, in a registry of their own
// rather than the global default: each Gateway (in tests, many) gets a fresh
// one, with no "duplicate registration" panics.
//
// Every label has a small, fixed set of values. The route label is the
// configured pattern ("GET /users/{id}"), never the request path: a raw path
// label would make a new time series for every user id, which is the usual
// way to run a Prometheus server out of memory.
type metrics struct {
	registry  *prometheus.Registry
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	inFlight  prometheus.Gauge
	attempts  *prometheus.CounterVec
	retries   *prometheus.CounterVec
	rateLimit *prometheus.CounterVec
}

func newMetrics(version string) *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gatekeeper_requests_total",
			Help: "Requests handled, by route pattern, backend and status code.",
		}, []string{"route", "backend", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gatekeeper_request_duration_seconds",
			Help: "Time from receiving a request to finishing the response.",
			// 1 ms to 10 s: proxied calls are usually milliseconds, and the
			// longest backend timeouts are seconds.
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"route", "backend"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gatekeeper_requests_in_flight",
			Help: "Requests being handled right now.",
		}),
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gatekeeper_upstream_attempts_total",
			Help: "Tries at a backend instance, retries included, by outcome.",
		}, []string{"backend", "outcome"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gatekeeper_retries_total",
			Help: "Retries sent to another instance after a failed attempt.",
		}, []string{"backend"}),
		rateLimit: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gatekeeper_rate_limit_decisions_total",
			Help: "Rate limit decisions: allowed, refused, or store_error (the store could not be asked).",
		}, []string{"decision"}),
	}
	m.registry.MustRegister(m.requests, m.duration, m.inFlight, m.attempts, m.retries, m.rateLimit,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "gatekeeper_build_info",
			Help:        "Always 1; the version label says which build is running.",
			ConstLabels: prometheus.Labels{"version": version},
		}, func() float64 { return 1 }),
	)
	return m
}

// watchBackend adds gauges read at scrape time: the circuit breaker's state
// and each instance's health. Reading them live means they cannot drift
// from what the gateway is actually doing.
func (m *metrics) watchBackend(name string, pool *balancer.Pool, breaker *resilience.Breaker) {
	if breaker != nil {
		m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "gatekeeper_circuit_state",
			Help:        "Circuit breaker state: 0 closed, 1 open, 2 half-open.",
			ConstLabels: prometheus.Labels{"backend": name},
		}, func() float64 { return float64(breaker.State()) }))
	}
	for _, inst := range pool.Instances() {
		m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "gatekeeper_instance_healthy",
			Help:        "1 if the instance takes traffic (health checks pass and it is not ejected), else 0.",
			ConstLabels: prometheus.Labels{"backend": name, "instance": inst.URL.Host},
		}, func() float64 {
			if inst.Healthy(pool.Now()) {
				return 1
			}
			return 0
		}))
	}
}

func (m *metrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}
