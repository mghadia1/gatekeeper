package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/mghadia1/gatekeeper/internal/config"
)

func scrape(t *testing.T, gw *Gateway) string {
	t.Helper()
	rec := httptest.NewRecorder()
	gw.MetricsHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics answered %d", rec.Code)
	}
	return rec.Body.String()
}

func metricsGateway(t *testing.T, backend config.Backend, rateLimit *config.RateLimit) (*httptest.Server, *Gateway) {
	t.Helper()
	cfg := config.Config{
		Backends:  map[string]config.Backend{"users": backend},
		Routes:    []config.Route{{Match: "GET /users/{id}", Backend: "users"}},
		RateLimit: rateLimit,
	}
	gw, err := New(cfg, "1.2.3", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return srv, gw
}

func TestRequestsAreCountedByRoutePatternNotPath(t *testing.T) {
	users := testBackend(t, "users")
	srv, gw := metricsGateway(t, config.Backend{
		Name: "users", URLs: []*url.URL{mustURL(t, users.URL)}, Timeout: time.Second, EjectAfter: 3, EjectFor: time.Minute,
	}, nil)
	for _, path := range []string{"/users/1", "/users/2", "/users/3", "/nope", "/healthz"} {
		send(t, "GET", srv.URL+path, nil)
	}

	if got := testutil.ToFloat64(gw.metrics.requests.WithLabelValues("GET /users/{id}", "users", "200")); got != 3 {
		t.Errorf("requests for the route = %v, want 3 (three ids, one series)", got)
	}
	if got := testutil.ToFloat64(gw.metrics.requests.WithLabelValues(unmatched, "", "404")); got != 1 {
		t.Errorf("unmatched 404s = %v, want 1", got)
	}
	body := scrape(t, gw)
	for _, raw := range []string{"/users/1", "/users/2", "/nope"} {
		if strings.Contains(body, `"`+raw+`"`) {
			t.Errorf("a raw path %q became a label value: unbounded cardinality", raw)
		}
	}
	if got := testutil.CollectAndCount(gw.metrics.duration); got != 2 {
		t.Errorf("%d latency series, want 2 (the route, and unmatched)", got)
	}
	for _, want := range []string{
		`gatekeeper_build_info{version="1.2.3"} 1`,
		"gatekeeper_requests_in_flight 0",
		"go_goroutines",
		`gatekeeper_request_duration_seconds_bucket{backend="users",route="GET /users/{id}",le="0.001"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
}

func TestAttemptsRetriesAndInstanceHealthAreExported(t *testing.T) {
	alive := testBackend(t, "users")
	dead := closedURL(t)
	srv, gw := metricsGateway(t, config.Backend{
		Name: "users", URLs: []*url.URL{mustURL(t, alive.URL), mustURL(t, dead)},
		Timeout: time.Second, EjectAfter: 2, EjectFor: time.Minute, Retries: 1,
	}, nil)
	for range 6 {
		send(t, "GET", srv.URL+"/users/1", nil)
	}
	failures := testutil.ToFloat64(gw.metrics.attempts.WithLabelValues("users", "failure"))
	retries := testutil.ToFloat64(gw.metrics.retries.WithLabelValues("users"))
	successes := testutil.ToFloat64(gw.metrics.attempts.WithLabelValues("users", "success"))
	if failures != 2 || retries != 2 || successes != 6 {
		t.Errorf("attempts: %v failed, %v retried, %v succeeded; want 2, 2, 6 (the dead instance is ejected after 2)",
			failures, retries, successes)
	}
	body := scrape(t, gw)
	deadHost := mustURL(t, dead).Host
	if !strings.Contains(body, `gatekeeper_instance_healthy{backend="users",instance="`+deadHost+`"} 0`) {
		t.Errorf("the ejected instance is not reported unhealthy:\n%s", grep(body, "instance_healthy"))
	}
	if !strings.Contains(body, `gatekeeper_instance_healthy{backend="users",instance="`+mustURL(t, alive.URL).Host+`"} 1`) {
		t.Errorf("the live instance is not reported healthy:\n%s", grep(body, "instance_healthy"))
	}
}

func TestCircuitStateIsExported(t *testing.T) {
	busy := newCountingBackend(t, http.StatusServiceUnavailable)
	srv, gw := metricsGateway(t, config.Backend{
		Name: "users", URLs: []*url.URL{mustURL(t, busy.URL)}, Timeout: time.Second,
		EjectAfter: 1000, EjectFor: time.Minute,
		CircuitBreaker: &config.CircuitBreaker{FailureRatio: 0.5, MinRequests: 3, OpenFor: time.Minute},
	}, nil)
	if !strings.Contains(scrape(t, gw), `gatekeeper_circuit_state{backend="users"} 0`) {
		t.Fatal("a new breaker is not reported closed (0)")
	}
	for range 3 {
		send(t, "GET", srv.URL+"/users/1", nil)
	}
	if !strings.Contains(scrape(t, gw), `gatekeeper_circuit_state{backend="users"} 1`) {
		t.Fatal("the open breaker is not reported open (1)")
	}
}

func TestRateLimitDecisionsAreCounted(t *testing.T) {
	users := testBackend(t, "users")
	srv, gw := metricsGateway(t, config.Backend{
		Name: "users", URLs: []*url.URL{mustURL(t, users.URL)}, Timeout: time.Second, EjectAfter: 3, EjectFor: time.Minute,
	}, &config.RateLimit{RequestsPerSecond: 0.1, Burst: 2, Store: "memory", FailOpen: true})
	for range 5 {
		send(t, "GET", srv.URL+"/users/1", nil)
	}
	allowed := testutil.ToFloat64(gw.metrics.rateLimit.WithLabelValues("allowed"))
	refused := testutil.ToFloat64(gw.metrics.rateLimit.WithLabelValues("refused"))
	if allowed != 2 || refused != 3 {
		t.Fatalf("rate limit decisions: %v allowed, %v refused; want 2 and 3", allowed, refused)
	}
	if got := testutil.ToFloat64(gw.metrics.requests.WithLabelValues("GET /users/{id}", "users", "429")); got != 3 {
		t.Fatalf("429s counted = %v, want 3", got)
	}
}

func grep(text, needle string) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
