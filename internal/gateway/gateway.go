// Package gateway builds the gateway's HTTP handler from a configuration:
// the routes, a reverse proxy per route, request IDs and the access log.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/mghadia1/gatekeeper/internal/config"
)

// New returns the gateway handler for cfg.
//
// Routes are registered on a net/http ServeMux, so matching, wildcards and
// precedence ("the most specific pattern wins") are the standard library's,
// documented and tested there. Conflicting patterns, which ServeMux reports
// by panicking, come back here as an error, so a bad config fails at startup
// rather than taking the process down.
func New(cfg config.Config, version string, logger *slog.Logger) (http.Handler, error) {
	mux := http.NewServeMux()
	// Liveness only: it checks nothing else, so a backend outage never makes
	// Docker restart the gateway.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
	})

	transport := newTransport()
	for _, route := range cfg.Routes {
		backend := cfg.Backends[route.Backend]
		if err := register(mux, route.Match, newProxy(route, backend, transport, logger)); err != nil {
			return nil, fmt.Errorf("route %q: %w", route.Match, err)
		}
	}
	// Anything no route matches lands here, unless the config itself claims
	// "/". A catch-all also catches requests whose path matches a route but
	// whose method does not, which ServeMux would otherwise have answered
	// with 405; so check for that before answering 404.
	_ = register(mux, "/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allowed := allowedMethods(mux, r); len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
				"error":      "method not allowed",
				"path":       r.URL.Path,
				"request_id": requestID(r.Context()),
			})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":      "no route matches",
			"path":       r.URL.Path,
			"request_id": requestID(r.Context()),
		})
	}))

	return withRequestID(withAccessLog(mux, logger)), nil
}

// allowedMethods lists the methods for which some route other than the
// catch-all matches r's path.
func allowedMethods(mux *http.ServeMux, r *http.Request) []string {
	var allowed []string
	for _, method := range []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
	} {
		probe := r.Clone(r.Context())
		probe.Method = method
		if _, pattern := mux.Handler(probe); pattern != "" && pattern != "/" {
			allowed = append(allowed, method)
		}
	}
	return allowed
}

// register adds a pattern, turning ServeMux's panic on an invalid or
// conflicting pattern into an error.
func register(mux *http.ServeMux, pattern string, handler http.Handler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	mux.Handle(pattern, handler)
	return nil
}

// newTransport is shared by every route. Go's default transport keeps only
// 2 idle connections per host; a proxy sending many concurrent requests to
// the same few backends would keep opening and closing connections, so this
// keeps far more.
func newTransport() *http.Transport {
	return &http.Transport{
		// Not http.ProxyFromEnvironment: a gateway should not quietly send
		// backend traffic through whatever HTTP_PROXY the host has set.
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// newProxy forwards a route's requests to its backend, within the backend's
// timeout.
func newProxy(route config.Route, backend config.Backend, transport http.RoundTripper, logger *slog.Logger) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			if route.StripPrefix != "" {
				path := strings.TrimPrefix(pr.Out.URL.Path, route.StripPrefix)
				if !strings.HasPrefix(path, "/") {
					path = "/" + path
				}
				pr.Out.URL.Path = path
				// Rebuilt from Path; an escaped form would no longer match.
				pr.Out.URL.RawPath = ""
			}
			// Joins the backend URL's path with the request's, and sends the
			// backend's own host name as Host.
			pr.SetURL(backend.URL)
			// X-Forwarded-For, -Host and -Proto, so the backend knows who
			// really asked.
			pr.SetXForwarded()
			pr.Out.Header.Set(requestIDHeader, requestID(pr.In.Context()))
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			info := infoFrom(r.Context())
			switch {
			case errors.Is(r.Context().Err(), context.DeadlineExceeded):
				info.err = err
				writeJSON(w, http.StatusGatewayTimeout, map[string]string{
					"error":      fmt.Sprintf("backend %q did not answer within %s", backend.Name, backend.Timeout),
					"request_id": requestID(r.Context()),
				})
			case errors.Is(r.Context().Err(), context.Canceled):
				// The client went away; there is no one to answer. 499 is
				// nginx's convention for this, recorded for the log only.
				info.err = err
				info.status = statusClientClosedRequest
			default:
				info.err = err
				writeJSON(w, http.StatusBadGateway, map[string]string{
					"error":      fmt.Sprintf("backend %q is unreachable", backend.Name),
					"request_id": requestID(r.Context()),
				})
			}
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := infoFrom(r.Context())
		info.route = route.Match
		info.backend = backend.Name
		// The timeout covers the whole exchange with the backend, including
		// copying its response body to the client.
		ctx, cancel := context.WithTimeout(r.Context(), backend.Timeout)
		defer cancel()
		proxy.ServeHTTP(w, r.WithContext(ctx))
	})
}

// statusClientClosedRequest is nginx's 499: the client disconnected before
// the gateway could answer. It is never sent; it only appears in the log.
const statusClientClosedRequest = 499
