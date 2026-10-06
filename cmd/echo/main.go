// Command echo is a test backend: it answers every request with a JSON
// description of what reached it, so a gateway's routing can be checked from
// outside.
//
//	GET /anything          {"backend": NAME, "method": ..., "path": ..., ...}
//	GET /anything?sleep=2s waits first (to exercise timeouts)
//	GET /status/503        answers with that status
//	echo healthcheck       exit 0 if the local echo answers /healthz
//
// ECHO_NAME names it in responses; ECHO_LISTEN sets the address (":9000").
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/mghadia1/gatekeeper/internal/server"
)

func main() {
	listen := envOr("ECHO_LISTEN", ":9000")
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		_, port, _ := net.SplitHostPort(listen)
		if err := server.Probe("http://127.0.0.1:"+port+"/healthz", 2*time.Second); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := server.Serve(ctx, ln, handler(envOr("ECHO_NAME", "echo")), 5*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func handler(name string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/status/{code}", func(w http.ResponseWriter, r *http.Request) {
		code, err := strconv.Atoi(r.PathValue("code"))
		if err != nil || code < 200 || code > 599 {
			http.Error(w, "bad status code", http.StatusBadRequest)
			return
		}
		w.WriteHeader(code)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if delay, err := time.ParseDuration(r.URL.Query().Get("sleep")); err == nil && delay > 0 {
			select {
			case <-time.After(min(delay, time.Minute)):
			case <-r.Context().Done():
				return
			}
		}
		body, _ := io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backend":        name,
			"method":         r.Method,
			"path":           r.URL.Path,
			"query":          r.URL.RawQuery,
			"host":           r.Host,
			"request_id":     r.Header.Get("X-Request-ID"),
			"forwarded_for":  r.Header.Get("X-Forwarded-For"),
			"forwarded_host": r.Header.Get("X-Forwarded-Host"),
			"body_bytes":     body,
		})
	})
	return mux
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
