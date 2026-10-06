package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEchoDescribesTheRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/users/7?x=1", strings.NewReader("hello"))
	req.Header.Set("X-Request-ID", "abc")
	handler("users").ServeHTTP(rec, req)

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["backend"] != "users" || got["method"] != "POST" || got["path"] != "/users/7" ||
		got["query"] != "x=1" || got["request_id"] != "abc" || got["body_bytes"] != float64(5) {
		t.Fatalf("echo = %v", got)
	}
}

func TestEchoStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	handler("x").ServeHTTP(rec, httptest.NewRequest("GET", "/status/503", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	rec = httptest.NewRecorder()
	handler("x").ServeHTTP(rec, httptest.NewRequest("GET", "/status/abc", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad code: status = %d, want 400", rec.Code)
	}
}
