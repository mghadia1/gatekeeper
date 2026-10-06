package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestServeFinishesRequestsInFlightOnShutdown(t *testing.T) {
	ln := listen(t)
	url := "http://" + ln.Addr().String()
	started := make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, "finished")
	})

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, slow, 5*time.Second) }()

	got := make(chan string, 1)
	go func() {
		resp, err := http.Get(url)
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		got <- string(body)
	}()

	<-started
	cancel() // shut down while the request is in flight

	if body := <-got; body != "finished" {
		t.Fatalf("in-flight request got %q, want it to finish", body)
	}
	if err := <-served; err != nil {
		t.Fatalf("Serve returned %v after a clean shutdown, want nil", err)
	}
	if _, err := http.Get(url); err == nil {
		t.Fatal("the server still accepts connections after shutdown")
	}
}

func TestServeGivesUpOnRequestsThatOutliveTheTimeout(t *testing.T) {
	ln := listen(t)
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	stuck := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	})

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, stuck, 100*time.Millisecond) }()
	go func() { _, _ = http.Get("http://" + ln.Addr().String()) }()

	<-started
	cancel()
	select {
	case err := <-served:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Serve = %v, want a shutdown deadline error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not give up after the shutdown timeout")
	}
}

func TestProbe(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
		}
	}))
	defer ok.Close()
	if err := Probe(ok.URL+"/healthz", time.Second); err != nil {
		t.Errorf("Probe of a healthy server = %v, want nil", err)
	}
	if err := Probe(ok.URL+"/missing", time.Second); err == nil {
		t.Error("Probe of a 404 returned nil")
	}

	ln := listen(t)
	addr := ln.Addr().String()
	ln.Close() // nothing listens here now
	if err := Probe("http://"+addr+"/healthz", time.Second); err == nil {
		t.Error("Probe of a closed port returned nil")
	}
}
