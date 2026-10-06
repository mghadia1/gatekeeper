package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestQuantile(t *testing.T) {
	values := make([]time.Duration, 100)
	for i := range values {
		values[i] = time.Duration(i+1) * time.Millisecond // 1..100 ms
	}
	for q, want := range map[float64]time.Duration{0.5: 50 * time.Millisecond, 0.99: 99 * time.Millisecond, 1: 100 * time.Millisecond} {
		if got := quantile(values, q); got != want {
			t.Errorf("quantile(%v) = %v, want %v", q, got, want)
		}
	}
	if quantile(nil, 0.5) != 0 {
		t.Error("quantile of nothing should be 0")
	}
}

func TestSendsAtTheTargetRateAndAlternatesURLs(t *testing.T) {
	var a, b atomic.Int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { a.Add(1) }))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.Add(1)
		if r.Header.Get("X-API-Key") != "k" {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srvB.Close()

	res, err := run(context.Background(), []string{srvA.URL, srvB.URL}, 200, 500*time.Millisecond, time.Second, "X-API-Key: k", 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests < 190 || res.Requests > 210 {
		t.Errorf("%d requests measured in 1 s at 200/s", res.Requests)
	}
	if res.AchievedRate < 180 || res.AchievedRate > 220 {
		t.Errorf("achieved %.0f/s, want about 200", res.AchievedRate)
	}
	if res.Statuses["200"] != res.Requests || res.Errors != 0 {
		t.Errorf("statuses %v, errors %d: the header did not reach the server, or requests failed", res.Statuses, res.Errors)
	}
	if diff := a.Load() - b.Load(); diff < -1 || diff > 1 {
		t.Errorf("URLs used %d and %d times, want alternating", a.Load(), b.Load())
	}
}

func TestLatencyIsMeasuredFromWhenTheRequestWasDue(t *testing.T) {
	// The server answers one request at a time and takes 50 ms each. At 100
	// requests per second, requests queue up: a closed-loop tool would report
	// about 50 ms each, but each request here waits for the ones before it,
	// and its latency must include that wait.
	var mu = make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu <- struct{}{}
		time.Sleep(50 * time.Millisecond)
		<-mu
	}))
	defer srv.Close()
	res, err := run(context.Background(), []string{srv.URL}, 100, 0, 500*time.Millisecond, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if res.LatencyMs["max"] < 500 {
		t.Fatalf("max latency %.0f ms; 50 requests needing 50 ms each in turn should queue well past 500 ms", res.LatencyMs["max"])
	}
}
