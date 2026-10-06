// Command gatekeeper is an HTTP API gateway.
//
//	gatekeeper              run the gateway
//	gatekeeper healthcheck  exit 0 if the local gateway answers /healthz
//
// The second form is for the container health check: the image is
// distroless, with no shell or curl to run one.
//
// Configuration comes from the environment:
//
//	GATEKEEPER_CONFIG            routes and backends, a JSON file (default
//	                             "/etc/gatekeeper/config.json"; see
//	                             internal/config)
//	GATEKEEPER_LISTEN            address to listen on (default ":8080")
//	GATEKEEPER_ADMIN_LISTEN      address of the internal admin port, which
//	                             serves /metrics (default ":9091"); keep it
//	                             off the public network
//	GATEKEEPER_SHUTDOWN_TIMEOUT  how long to let requests in flight finish
//	                             on SIGTERM (default "10s")
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mghadia1/gatekeeper/internal/config"
	"github.com/mghadia1/gatekeeper/internal/gateway"
	"github.com/mghadia1/gatekeeper/internal/server"
)

// version is set at build time: -ldflags "-X main.version=...".
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "gatekeeper")

	listen := envOr("GATEKEEPER_LISTEN", ":8080")
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := server.Probe(localURL(listen)+"/healthz", 2*time.Second); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		return
	}

	if err := run(listen, logger); err != nil {
		logger.Error("gatekeeper stopped", "error", err)
		os.Exit(1)
	}
}

func run(listen string, logger *slog.Logger) error {
	shutdownTimeout, err := time.ParseDuration(envOr("GATEKEEPER_SHUTDOWN_TIMEOUT", "10s"))
	if err != nil {
		return fmt.Errorf("GATEKEEPER_SHUTDOWN_TIMEOUT: %w", err)
	}

	configPath := envOr("GATEKEEPER_CONFIG", "/etc/gatekeeper/config.json")
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	gw, err := gateway.New(cfg, version, logger)
	if err != nil {
		return fmt.Errorf("%s: %w", configPath, err)
	}
	defer gw.Close()

	// SIGTERM is what `docker stop` sends; SIGINT is Ctrl-C.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	// Health checks run alongside the server. They stop when it does, for
	// whatever reason: cancel first, then wait. (Deferred calls run in
	// reverse order, so relying on stop() above to cancel would wait for the
	// checks before cancelling them, and hang if Serve failed early.)
	checksCtx, cancelChecks := context.WithCancel(ctx)
	checks := make(chan struct{})
	go func() { gw.Run(checksCtx); close(checks) }()
	defer func() { cancelChecks(); <-checks }()
	logger.Info("listening", "address", ln.Addr().String(), "version", version,
		"routes", len(cfg.Routes), "backends", len(cfg.Backends))
	// Metrics on their own port, shut down with the main server.
	adminLn, err := net.Listen("tcp", envOr("GATEKEEPER_ADMIN_LISTEN", ":9091"))
	if err != nil {
		ln.Close()
		return err
	}
	admin := http.NewServeMux()
	admin.Handle("GET /metrics", gw.MetricsHandler())
	adminDone := make(chan error, 1)
	go func() { adminDone <- server.Serve(ctx, adminLn, admin, shutdownTimeout) }()
	logger.Info("admin listening", "address", adminLn.Addr().String())

	if err := server.Serve(ctx, ln, gw, shutdownTimeout); err != nil {
		return err
	}
	if err := <-adminDone; err != nil {
		return fmt.Errorf("admin: %w", err)
	}
	logger.Info("shut down cleanly")
	return nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// localURL turns a listen address such as ":8080" or "0.0.0.0:8080" into a
// URL that reaches it from the same machine.
func localURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = strings.TrimPrefix(listen, ":")
	}
	return "http://" + net.JoinHostPort(host, port)
}
