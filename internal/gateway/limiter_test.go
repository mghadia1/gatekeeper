package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mghadia1/gatekeeper/internal/config"
)

func limitedGateway(t *testing.T, limit config.RateLimit) (*httptest.Server, *logBuffer) {
	t.Helper()
	backend := testBackend(t, "svc")
	cfg := config.Config{
		Backends: map[string]config.Backend{"svc": {
			Name: "svc", URLs: []*url.URL{mustURL(t, backend.URL)}, Timeout: time.Second,
			EjectAfter: 3, EjectFor: time.Minute,
		}},
		Routes:    []config.Route{{Match: "/api/", Backend: "svc"}},
		RateLimit: &limit,
	}
	logs := &logBuffer{}
	gw, err := New(cfg, "test", slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return srv, logs
}

func request(t *testing.T, url, apiKey string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

var slowLimit = config.RateLimit{RequestsPerSecond: 0.5, Burst: 3, KeyHeader: "X-API-Key", Store: "memory", FailOpen: true}

func TestTheBurstPassesThenClientsGet429WithRetryAfter(t *testing.T) {
	srv, logs := limitedGateway(t, slowLimit)
	for i, remaining := range []string{"2", "1", "0"} {
		resp := request(t, srv.URL+"/api/x", "key-a")
		if resp.StatusCode != http.StatusOK || resp.Header.Get("X-RateLimit-Remaining") != remaining {
			t.Fatalf("request %d: %d with %s remaining, want 200 with %s",
				i+1, resp.StatusCode, resp.Header.Get("X-RateLimit-Remaining"), remaining)
		}
	}

	req, _ := http.NewRequest("GET", srv.URL+"/api/x", nil)
	req.Header.Set("X-API-Key", "key-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusTooManyRequests || body["error"] != "rate limit exceeded" {
		t.Fatalf("request 4: %d %v, want a JSON 429", resp.StatusCode, body)
	}
	// One token takes 2 s at 0.5/s: Retry-After rounds up to whole seconds.
	if retry, _ := strconv.Atoi(resp.Header.Get("Retry-After")); retry != 2 {
		t.Errorf("Retry-After = %q, want 2", resp.Header.Get("Retry-After"))
	}
	if resp.Header.Get("X-RateLimit-Limit") != "3" {
		t.Errorf("X-RateLimit-Limit = %q, want 3", resp.Header.Get("X-RateLimit-Limit"))
	}
	if !strings.Contains(logs.String(), `"rate_limited":true`) || !strings.Contains(logs.String(), `"status":429`) {
		t.Error("the refused request was not logged as rate limited")
	}
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"status":429`) && !strings.Contains(line, `"route":"/api/"`) {
			t.Errorf("a refused request was logged without its route: %s", line)
		}
	}
	if strings.Contains(logs.String(), "key-a") {
		t.Error("the API key appears in the logs; it should be hashed")
	}
}

func TestEachAPIKeyHasItsOwnLimit(t *testing.T) {
	srv, _ := limitedGateway(t, slowLimit)
	for range 3 {
		request(t, srv.URL+"/api/x", "key-a")
	}
	if request(t, srv.URL+"/api/x", "key-a").StatusCode != http.StatusTooManyRequests {
		t.Fatal("key-a was not limited after its burst")
	}
	if request(t, srv.URL+"/api/x", "key-b").StatusCode != http.StatusOK {
		t.Fatal("key-b was limited because of key-a")
	}
	if request(t, srv.URL+"/api/x", "").StatusCode != http.StatusOK {
		t.Fatal("a client without a key (limited by IP) was limited because of key-a")
	}
}

func TestHealthzAnd404AreNotLimited(t *testing.T) {
	srv, _ := limitedGateway(t, config.RateLimit{RequestsPerSecond: 0.1, Burst: 1, Store: "memory", FailOpen: true})
	for range 5 {
		if request(t, srv.URL+"/healthz", "").StatusCode != http.StatusOK {
			t.Fatal("/healthz was rate limited")
		}
		if request(t, srv.URL+"/nothing", "").StatusCode != http.StatusNotFound {
			t.Fatal("an unmatched path was rate limited")
		}
	}
}

func TestStoreOutageFailsOpenByDefaultAndLogsSparingly(t *testing.T) {
	srv, logs := limitedGateway(t, config.RateLimit{
		RequestsPerSecond: 1, Burst: 1, KeyHeader: "X-API-Key", Store: "redis",
		RedisURL: "redis://127.0.0.1:1/0", FailOpen: true,
	})
	for i := range 5 {
		started := time.Now()
		if resp := request(t, srv.URL+"/api/x", "k"); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d with Redis down: %d, want 200 (fail open)", i+1, resp.StatusCode)
		}
		// Failing open must also be fast: no retries, no backoff per request.
		if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
			t.Fatalf("request %d with Redis down took %v; the outage is adding latency", i+1, elapsed)
		}
	}
	if n := strings.Count(logs.String(), "rate limit store unavailable"); n != 1 {
		t.Fatalf("store outage logged %d times for 5 requests, want once", n)
	}
}

func TestStoreOutageFailsClosedWhenConfigured(t *testing.T) {
	srv, logs := limitedGateway(t, config.RateLimit{
		RequestsPerSecond: 1, Burst: 1, Store: "redis", RedisURL: "redis://127.0.0.1:1/0", FailOpen: false,
	})
	if resp := request(t, srv.URL+"/api/x", ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("with Redis down and fail-closed: %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(logs.String(), "refusing requests with 503") {
		t.Error("the fail-closed policy was not logged")
	}
}
