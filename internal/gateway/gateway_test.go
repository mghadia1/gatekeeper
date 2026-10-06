package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mghadia1/gatekeeper/internal/config"
)

// seen is what a test backend reports about the request that reached it.
type seen struct {
	Backend   string `json:"backend"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Query     string `json:"query"`
	Host      string `json:"host"`
	RequestID string `json:"request_id"`
	Forwarded string `json:"forwarded_for"`
	FwdHost   string `json:"forwarded_host"`
	Body      string `json:"body"`
}

func testBackend(t *testing.T, name string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay := r.URL.Query().Get("sleep"); delay != "" {
			d, _ := time.ParseDuration(delay)
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set(requestIDHeader, r.Header.Get(requestIDHeader)) // echo it back
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(seen{
			Backend: name, Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Host: r.Host, RequestID: r.Header.Get(requestIDHeader),
			Forwarded: r.Header.Get("X-Forwarded-For"), FwdHost: r.Header.Get("X-Forwarded-Host"),
			Body: string(body),
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// fixture is a gateway in front of two test backends and one that is down.
type fixture struct {
	gateway *httptest.Server
	logs    *logBuffer
	users   *httptest.Server
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	users, orders := testBackend(t, "users"), testBackend(t, "orders")
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	downURL := "http://" + closed.Addr().String()
	closed.Close() // nothing listens there now

	cfg := config.Config{
		Backends: map[string]config.Backend{
			"users":  {Name: "users", URLs: []*url.URL{mustURL(t, users.URL)}, Timeout: 2 * time.Second},
			"strict": {Name: "strict", URLs: []*url.URL{mustURL(t, users.URL)}, Timeout: 100 * time.Millisecond},
			"orders": {Name: "orders", URLs: []*url.URL{mustURL(t, orders.URL)}, Timeout: 2 * time.Second},
			"down":   {Name: "down", URLs: []*url.URL{mustURL(t, downURL)}, Timeout: 2 * time.Second},
		},
		Routes: []config.Route{
			{Match: "GET /users/{id}", Backend: "users"},
			{Match: "GET /users/me", Backend: "orders"}, // more specific: wins over {id}
			{Match: "POST /users", Backend: "users"},
			{Match: "/api/orders/", Backend: "orders", StripPrefix: "/api"},
			{Match: "GET /slow", Backend: "strict"},
			{Match: "/down/", Backend: "down"},
		},
	}
	logs := &logBuffer{}
	handler, err := New(cfg, "test", slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(handler)
	t.Cleanup(gw.Close)
	return fixture{gateway: gw, logs: logs, users: users}
}

func (f fixture) do(t *testing.T, method, path string, body io.Reader, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.gateway.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func decodeSeen(t *testing.T, data []byte) seen {
	t.Helper()
	var s seen
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("backend response %q is not JSON: %v", data, err)
	}
	return s
}

func TestRoutesToTheRightBackendWithPathAndQuery(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		method, path, wantBackend, wantPath, wantQuery string
	}{
		{"GET", "/users/42?expand=true", "users", "/users/42", "expand=true"},
		{"GET", "/users/me", "orders", "/users/me", ""},
		{"GET", "/api/orders/7/items", "orders", "/orders/7/items", ""},
		{"DELETE", "/api/orders/7", "orders", "/orders/7", ""},
	}
	for _, tt := range tests {
		resp, data := f.do(t, tt.method, tt.path, nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s: status %d, body %s", tt.method, tt.path, resp.StatusCode, data)
			continue
		}
		got := decodeSeen(t, data)
		if got.Backend != tt.wantBackend || got.Path != tt.wantPath || got.Query != tt.wantQuery {
			t.Errorf("%s %s reached %s at %q?%q, want %s at %q?%q",
				tt.method, tt.path, got.Backend, got.Path, got.Query, tt.wantBackend, tt.wantPath, tt.wantQuery)
		}
	}
}

func TestForwardsTheBodyAndHostHeaders(t *testing.T) {
	f := newFixture(t)
	resp, data := f.do(t, "POST", "/users", strings.NewReader(`{"name":"ada"}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got := decodeSeen(t, data)
	if got.Method != "POST" || got.Body != `{"name":"ada"}` {
		t.Errorf("backend saw %s with body %q", got.Method, got.Body)
	}
	if got.Host != mustURL(t, f.users.URL).Host {
		t.Errorf("backend saw Host %q, want its own host %q", got.Host, mustURL(t, f.users.URL).Host)
	}
	if got.Forwarded != "127.0.0.1" || got.FwdHost == "" {
		t.Errorf("X-Forwarded-For %q / X-Forwarded-Host %q not set", got.Forwarded, got.FwdHost)
	}
}

func TestUnmatchedPathIsJSON404AndWrongMethodIs405(t *testing.T) {
	f := newFixture(t)
	resp, data := f.do(t, "GET", "/nothing/here", nil, nil)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(data), "no route matches") {
		t.Errorf("GET /nothing/here: %d %s, want a JSON 404", resp.StatusCode, data)
	}
	resp, _ = f.do(t, "PUT", "/users/42", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT /users/42: status %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("405 Allow header = %q, want it to list GET", allow)
	}
}

func TestRequestIDIsCreatedForwardedAndReturnedOnce(t *testing.T) {
	f := newFixture(t)

	resp, data := f.do(t, "GET", "/users/1", nil, nil)
	returned := resp.Header.Values(requestIDHeader)
	if len(returned) != 1 || len(returned[0]) != 32 {
		t.Fatalf("X-Request-ID returned as %q, want exactly one 32-character id", returned)
	}
	if got := decodeSeen(t, data).RequestID; got != returned[0] {
		t.Errorf("backend saw request id %q, client got %q", got, returned[0])
	}

	resp, data = f.do(t, "GET", "/users/1", nil, map[string]string{requestIDHeader: "trace-abc"})
	if resp.Header.Get(requestIDHeader) != "trace-abc" || decodeSeen(t, data).RequestID != "trace-abc" {
		t.Errorf("a safe caller id was not kept end to end")
	}

	resp, _ = f.do(t, "GET", "/users/1", nil, map[string]string{requestIDHeader: "bad id; drop table"})
	if id := resp.Header.Get(requestIDHeader); id == "bad id; drop table" || len(id) != 32 {
		t.Errorf("an unsafe caller id was not replaced: %q", id)
	}
}

func TestUnreachableBackendIs502(t *testing.T) {
	f := newFixture(t)
	resp, data := f.do(t, "GET", "/down/anything", nil, nil)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(data), "unreachable") {
		t.Fatalf("status %d, body %s; want 502", resp.StatusCode, data)
	}
	if resp.Header.Get(requestIDHeader) == "" {
		t.Error("the 502 has no X-Request-ID")
	}
}

func TestSlowBackendIs504AtItsTimeout(t *testing.T) {
	f := newFixture(t)
	started := time.Now()
	resp, data := f.do(t, "GET", "/slow?sleep=2s", nil, nil)
	elapsed := time.Since(started)
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status %d, body %s; want 504", resp.StatusCode, data)
	}
	if elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Fatalf("504 after %v, want it at the 100 ms backend timeout", elapsed)
	}
}

func TestHealthzIsServedByTheGateway(t *testing.T) {
	f := newFixture(t)
	resp, data := f.do(t, "GET", "/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(data), `"version":"test"`) {
		t.Fatalf("GET /healthz: %d %s", resp.StatusCode, data)
	}
}

func TestEachRequestIsLoggedOnceWithRouteAndOutcome(t *testing.T) {
	f := newFixture(t)
	resp, _ := f.do(t, "GET", "/users/9?token=secret", nil, map[string]string{requestIDHeader: "log-check"})
	f.do(t, "GET", "/down/x", nil, nil)
	f.do(t, "GET", "/healthz", nil, nil) // logged at DEBUG: not in the INFO output

	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		lines = append(lines, entry)
	}
	if len(lines) != 2 {
		t.Fatalf("%d log lines, want 2 (healthz is DEBUG): %v", len(lines), lines)
	}
	ok := lines[0]
	if ok["request_id"] != "log-check" || ok["route"] != "GET /users/{id}" || ok["backend"] != "users" ||
		ok["status"] != float64(200) || resp.StatusCode != 200 {
		t.Errorf("success line = %v", ok)
	}
	if strings.Contains(f.logs.String(), "secret") {
		t.Error("the query string (which may hold secrets) was logged")
	}
	failed := lines[1]
	if failed["status"] != float64(502) || failed["backend"] != "down" || failed["error"] == nil {
		t.Errorf("failure line = %v, want status 502, backend down and an error", failed)
	}
}

func TestConflictingRoutesAreAConfigErrorNotAPanic(t *testing.T) {
	backend := config.Backend{Name: "a", URLs: []*url.URL{mustURL(t, "http://a")}, Timeout: time.Second}
	cfg := config.Config{
		Backends: map[string]config.Backend{"a": backend},
		Routes: []config.Route{
			{Match: "GET /items/{id}", Backend: "a"},
			{Match: "GET /items/{name}", Backend: "a"}, // same shape: ServeMux panics
		},
	}
	_, err := New(cfg, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "GET /items/{name}") {
		t.Fatalf("New = %v, want an error naming the conflicting route", err)
	}
}

func TestAConfigMayClaimTheRootItself(t *testing.T) {
	users := testBackend(t, "users")
	cfg := config.Config{
		Backends: map[string]config.Backend{"u": {Name: "u", URLs: []*url.URL{mustURL(t, users.URL)}, Timeout: time.Second}},
		Routes:   []config.Route{{Match: "/", Backend: "u"}},
	}
	handler, err := New(cfg, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New = %v, want the config's own \"/\" route to be allowed", err)
	}
	gw := httptest.NewServer(handler)
	defer gw.Close()
	resp, err := http.Get(gw.URL + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /anything with a \"/\" route: status %d, want 200 from the backend", resp.StatusCode)
	}
}
