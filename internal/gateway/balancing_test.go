package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mghadia1/gatekeeper/internal/config"
)

// lbGateway puts a gateway in front of one backend made of the given
// instance URLs.
func lbGateway(t *testing.T, backend config.Backend) (*httptest.Server, *Gateway, *logBuffer) {
	t.Helper()
	cfg := config.Config{
		Backends: map[string]config.Backend{"svc": backend},
		Routes:   []config.Route{{Match: "/", Backend: "svc"}},
	}
	logs := &logBuffer{}
	gw, err := New(cfg, "test", slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return srv, gw, logs
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var s seen
	_ = json.Unmarshal(body, &s)
	return resp.StatusCode, s.Backend
}

func closedURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr
}

func TestRequestsAlternateAcrossInstances(t *testing.T) {
	a, b := testBackend(t, "a"), testBackend(t, "b")
	srv, _, _ := lbGateway(t, config.Backend{
		Name: "svc", URLs: []*url.URL{mustURL(t, a.URL), mustURL(t, b.URL)},
		Timeout: time.Second, EjectAfter: 3, EjectFor: time.Minute,
	})
	counts := map[string]int{}
	for range 20 {
		status, who := get(t, srv.URL+"/x")
		if status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
		counts[who]++
	}
	if counts["a"] != 10 || counts["b"] != 10 {
		t.Fatalf("instances served %v, want 10 each", counts)
	}
}

func TestADeadInstanceCostsAtMostEjectAfterFailures(t *testing.T) {
	alive := testBackend(t, "alive")
	srv, _, logs := lbGateway(t, config.Backend{
		Name: "svc", URLs: []*url.URL{mustURL(t, alive.URL), mustURL(t, closedURL(t))},
		Timeout: time.Second, EjectAfter: 2, EjectFor: time.Minute,
	})

	failed := 0
	for range 40 {
		status, who := get(t, srv.URL+"/x")
		switch {
		case status == http.StatusBadGateway:
			failed++
		case status != http.StatusOK || who != "alive":
			t.Fatalf("unexpected answer: %d from %q", status, who)
		}
	}
	// No retries yet (Milestone 4): each request to the dead instance fails
	// until passive detection has seen EjectAfter of them.
	if failed != 2 {
		t.Fatalf("%d of 40 requests failed, want exactly EjectAfter (2)", failed)
	}
	if !strings.Contains(logs.String(), "instance ejected after failed requests") {
		t.Error("the ejection was not logged")
	}
}

func TestAnInstanceAnswering503IsEjectedToo(t *testing.T) {
	alive := testBackend(t, "alive")
	var overloaded atomic.Int32
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		overloaded.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(busy.Close)
	srv, _, _ := lbGateway(t, config.Backend{
		Name: "svc", URLs: []*url.URL{mustURL(t, alive.URL), mustURL(t, busy.URL)},
		Timeout: time.Second, EjectAfter: 3, EjectFor: time.Minute,
	})
	for range 30 {
		get(t, srv.URL+"/x")
	}
	if got := overloaded.Load(); got != 3 {
		t.Fatalf("the instance answering 503 got %d requests, want 3 before ejection", got)
	}
}

func TestAnInstanceAnswering500StaysInRotation(t *testing.T) {
	var errors atomic.Int32
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		errors.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	alive := testBackend(t, "alive")
	srv, _, _ := lbGateway(t, config.Backend{
		Name: "svc", URLs: []*url.URL{mustURL(t, alive.URL), mustURL(t, failing.URL)},
		Timeout: time.Second, EjectAfter: 2, EjectFor: time.Minute,
	})
	for range 20 {
		get(t, srv.URL+"/x")
	}
	// A 500 is about one request, not the instance: it keeps its share.
	if got := errors.Load(); got != 10 {
		t.Fatalf("the instance answering 500 got %d of 20 requests, want 10", got)
	}
}

func TestHealthChecksRemoveAnInstanceBeforeAnyRequestFails(t *testing.T) {
	alive := testBackend(t, "alive")
	var healthy atomic.Bool
	healthy.Store(true)
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(seen{Backend: "sick"})
	}))
	t.Cleanup(sick.Close)

	srv, gw, logs := lbGateway(t, config.Backend{
		Name: "svc", URLs: []*url.URL{mustURL(t, alive.URL), mustURL(t, sick.URL)},
		Timeout: time.Second, EjectAfter: 3, EjectFor: time.Minute,
		HealthCheck: &config.HealthCheck{
			Path: "/healthz", Interval: 10 * time.Millisecond, Timeout: time.Second,
			UnhealthyAfter: 2, HealthyAfter: 2,
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { gw.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	healthy.Store(false)
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(logs.String(), "marked unhealthy by health checks") {
		if time.Now().After(deadline) {
			t.Fatal("the health checks never marked the instance down")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for range 10 {
		if _, who := get(t, srv.URL+"/x"); who != "alive" {
			t.Fatalf("request went to %q after the health checks marked it down", who)
		}
	}

	healthy.Store(true)
	deadline = time.Now().Add(3 * time.Second)
	for !strings.Contains(logs.String(), "marked healthy by health checks") {
		if time.Now().After(deadline) {
			t.Fatal("the health checks never marked the instance up again")
		}
		time.Sleep(5 * time.Millisecond)
	}
	counts := map[string]int{}
	for range 10 {
		_, who := get(t, srv.URL+"/x")
		counts[who]++
	}
	if counts["sick"] == 0 {
		t.Fatalf("the recovered instance got no traffic: %v", counts)
	}
}

func TestTheAccessLogNamesTheInstance(t *testing.T) {
	a := testBackend(t, "a")
	srv, _, logs := lbGateway(t, config.Backend{
		Name: "svc", URLs: []*url.URL{mustURL(t, a.URL)}, Timeout: time.Second, EjectAfter: 3, EjectFor: time.Minute,
	})
	get(t, srv.URL+"/x")
	if want := `"instance":"` + mustURL(t, a.URL).Host + `"`; !strings.Contains(logs.String(), want) {
		t.Fatalf("access log %s does not contain %s", logs.String(), want)
	}
}
