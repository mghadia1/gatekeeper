package gateway

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mghadia1/gatekeeper/internal/config"
)

// countingBackend answers with the status in mode and counts requests.
type countingBackend struct {
	*httptest.Server
	mode     atomic.Int32
	requests atomic.Int32
}

func newCountingBackend(t *testing.T, status int) *countingBackend {
	t.Helper()
	b := &countingBackend{}
	b.mode.Store(int32(status))
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(int(b.mode.Load()))
	}))
	t.Cleanup(b.Close)
	return b
}

func resilientGateway(t *testing.T, backend config.Backend, maxBody int64) (*httptest.Server, *logBuffer) {
	t.Helper()
	cfg := config.Config{
		Backends:     map[string]config.Backend{"svc": backend},
		Routes:       []config.Route{{Match: "/", Backend: "svc"}},
		MaxBodyBytes: maxBody,
	}
	logs := &logBuffer{}
	gw, err := New(cfg, "test", slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return srv, logs
}

func send(t *testing.T, method, url string, body io.Reader) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func TestARetryHidesADeadInstance(t *testing.T) {
	alive := newCountingBackend(t, http.StatusOK)
	srv, logs := resilientGateway(t, config.Backend{
		Name: "svc", URLs: []*url.URL{mustURL(t, alive.URL), mustURL(t, closedURL(t))},
		Timeout: time.Second, EjectAfter: 2, EjectFor: time.Minute, Retries: 1,
	}, 0)
	for i := range 40 {
		if status := send(t, "GET", srv.URL+"/x", nil); status != http.StatusOK {
			t.Fatalf("request %d: %d; with one retry, a dead instance should cost no failures", i+1, status)
		}
	}
	if !strings.Contains(logs.String(), `"attempts":2`) {
		t.Error("no retried request was logged with attempts 2")
	}
}

func TestRequestsWithABodyAreNotRetried(t *testing.T) {
	alive := newCountingBackend(t, http.StatusOK)
	dead := closedURL(t)
	srv, _ := resilientGateway(t, config.Backend{
		Name: "svc", URLs: []*url.URL{mustURL(t, dead), mustURL(t, alive.URL)},
		Timeout: time.Second, EjectAfter: 100, EjectFor: time.Minute, Retries: 1,
	}, 0)
	statuses := map[int]int{}
	for range 4 { // round robin: requests 1 and 3 go to the dead instance first
		statuses[send(t, "POST", srv.URL+"/x", strings.NewReader("payload"))]++
	}
	if statuses[http.StatusBadGateway] != 2 || statuses[http.StatusOK] != 2 {
		t.Fatalf("POSTs with a body: %v, want 2 failed (not retried: a body cannot be sent twice) and 2 ok", statuses)
	}
}

func TestABodylessPostIsRetriedOnlyIfTheConnectionNeverOpened(t *testing.T) {
	alive := newCountingBackend(t, http.StatusOK)
	srv, _ := resilientGateway(t, config.Backend{
		Name: "svc", URLs: []*url.URL{mustURL(t, closedURL(t)), mustURL(t, alive.URL)},
		Timeout: time.Second, EjectAfter: 100, EjectFor: time.Minute, Retries: 1,
	}, 0)
	for range 4 {
		if status := send(t, "POST", srv.URL+"/x", nil); status != http.StatusOK {
			t.Fatalf("bodyless POST: %d, want 200: the dial failed, so the backend never saw it and a retry is safe", status)
		}
	}

	busy := newCountingBackend(t, http.StatusServiceUnavailable)
	srv, _ = resilientGateway(t, config.Backend{
		Name: "svc", URLs: []*url.URL{mustURL(t, busy.URL), mustURL(t, alive.URL)},
		Timeout: time.Second, EjectAfter: 100, EjectFor: time.Minute, Retries: 1,
	}, 0)
	statuses := map[int]int{}
	for range 4 {
		statuses[send(t, "POST", srv.URL+"/x", nil)]++
	}
	if statuses[http.StatusServiceUnavailable] != 2 {
		t.Fatalf("bodyless POSTs to a 503 instance: %v, want the 503s passed through (it saw them: no retry)", statuses)
	}
	if got := send(t, "GET", srv.URL+"/x", nil); got != http.StatusOK {
		t.Fatalf("GET after a 503: %d, want 200 (a safe method is retried)", got)
	}
}

func TestTheRetryBudgetCapsAmplification(t *testing.T) {
	a := newCountingBackend(t, http.StatusServiceUnavailable)
	b := newCountingBackend(t, http.StatusServiceUnavailable)
	srv, _ := resilientGateway(t, config.Backend{
		Name: "svc", URLs: []*url.URL{mustURL(t, a.URL), mustURL(t, b.URL)},
		Timeout: time.Second, EjectAfter: 1000, EjectFor: time.Minute, Retries: 5,
	}, 0)
	for range 100 {
		send(t, "GET", srv.URL+"/x", nil)
	}
	total := a.requests.Load() + b.requests.Load()
	t.Logf("100 requests, every attempt failing, retries 5: backends saw %d attempts", total)
	// Without a budget, 5 retries each would be 600 attempts. The budget
	// allows 20% of 100 requests plus 3/s over 10 s: 100 + 20 + 30.
	if total > 150 {
		t.Fatalf("backends saw %d attempts for 100 requests with every attempt failing, want at most 150", total)
	}
	if total <= 100 {
		t.Fatalf("backends saw %d attempts: no retries happened at all", total)
	}
}

func TestTheCircuitBreakerFailsFastThenProbesAndCloses(t *testing.T) {
	backend := newCountingBackend(t, http.StatusServiceUnavailable)
	srv, logs := resilientGateway(t, config.Backend{
		Name: "svc", URLs: []*url.URL{mustURL(t, backend.URL)},
		Timeout: time.Second, EjectAfter: 1000, EjectFor: time.Minute, Retries: 0,
		CircuitBreaker: &config.CircuitBreaker{FailureRatio: 0.5, MinRequests: 5, OpenFor: 300 * time.Millisecond},
	}, 0)

	for range 5 {
		send(t, "GET", srv.URL+"/x", nil) // 503s from the backend: the breaker counts them
	}
	seen := backend.requests.Load()
	started := time.Now()
	for range 20 {
		if status := send(t, "GET", srv.URL+"/x", nil); status != http.StatusServiceUnavailable {
			t.Fatalf("open breaker answered %d, want 503", status)
		}
	}
	if backend.requests.Load() != seen {
		t.Fatal("requests reached the backend while the circuit was open")
	}
	elapsed := time.Since(started)
	t.Logf("20 requests refused by the open circuit in %v", elapsed)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("20 requests against an open circuit took %v; they should be refused at once", elapsed)
	}
	if !strings.Contains(logs.String(), `"circuit_open":true`) {
		t.Error("refusals were not logged as circuit_open")
	}

	backend.mode.Store(http.StatusOK) // the backend recovers
	time.Sleep(300 * time.Millisecond)
	if status := send(t, "GET", srv.URL+"/x", nil); status != http.StatusOK {
		t.Fatalf("the half-open probe got %d, want 200", status)
	}
	for range 5 {
		if status := send(t, "GET", srv.URL+"/x", nil); status != http.StatusOK {
			t.Fatalf("after a successful probe: %d, want 200 (closed)", status)
		}
	}
}

func TestBodiesOverTheLimitAre413(t *testing.T) {
	backend := newCountingBackend(t, http.StatusOK)
	srv, _ := resilientGateway(t, config.Backend{
		Name: "svc", URLs: []*url.URL{mustURL(t, backend.URL)},
		Timeout: time.Second, EjectAfter: 3, EjectFor: time.Minute, Retries: 1,
	}, 1024)

	if status := send(t, "POST", srv.URL+"/x", bytes.NewReader(make([]byte, 2048))); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared 2 KiB body over a 1 KiB limit: %d, want 413", status)
	}
	if backend.requests.Load() != 0 {
		t.Fatal("a body over the declared limit reached the backend")
	}
	// No Content-Length (an io.Reader the client cannot measure): chunked.
	chunked := io.MultiReader(strings.NewReader(strings.Repeat("x", 2048)))
	if status := send(t, "POST", srv.URL+"/x", chunked); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked 2 KiB body: %d, want 413", status)
	}
	if status := send(t, "POST", srv.URL+"/x", strings.NewReader("small")); status != http.StatusOK {
		t.Fatalf("small body: %d, want 200", status)
	}
}
