// Package server holds the gateway's HTTP handler and its lifecycle:
// serving, graceful shutdown, and the probe the container health check uses.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Handler returns the gateway's HTTP handler. In Milestone 0 it has one
// route; proxying arrives in Milestone 1.
func Handler(version string) http.Handler {
	mux := http.NewServeMux()
	// Liveness only: the process is up and serving. It deliberately checks
	// nothing else, so a backend outage never makes Docker restart the
	// gateway.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The bodies written here are small maps of strings; encoding cannot fail
	// in a way the client could still be told about.
	_ = json.NewEncoder(w).Encode(body)
}

// Serve serves handler on ln until ctx is cancelled, then shuts down
// gracefully: it stops accepting connections and waits up to shutdownTimeout
// for requests in flight to finish. It returns nil after a clean shutdown.
func Serve(ctx context.Context, ln net.Listener, handler http.Handler, shutdownTimeout time.Duration) error {
	srv := &http.Server{
		Handler: handler,
		// A client that sends headers slowly cannot hold a connection open
		// forever (Slowloris).
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	select {
	case err := <-served:
		// Serve stopped by itself: the listener failed.
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	// After Shutdown, Serve returns http.ErrServerClosed; anything else is a
	// real error.
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// Probe returns nil if url answers 200 within timeout. The container image
// has no shell or curl, so its health check runs `gatekeeper healthcheck`,
// which calls this.
func Probe(url string, timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return nil
}
