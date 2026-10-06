// Package gateway builds the gateway's HTTP handler from a configuration:
// the routes, a reverse proxy per route, a load-balanced pool per backend,
// request IDs and the access log.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"github.com/mghadia1/gatekeeper/internal/balancer"
	"github.com/mghadia1/gatekeeper/internal/config"
	"github.com/mghadia1/gatekeeper/internal/resilience"
)

// Gateway is the gateway's HTTP handler plus the background work it needs:
// the health checks of each backend's instances.
type Gateway struct {
	handler http.Handler
	pools   []*balancer.Pool
	limiter *limiter // nil without rate limiting
}

// Close releases what the gateway holds open (the rate limiter's Redis
// connections).
func (g *Gateway) Close() error {
	if g.limiter == nil {
		return nil
	}
	return g.limiter.closer()
}

// ServeHTTP handles one request.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.handler.ServeHTTP(w, r) }

// Run runs the health checks until ctx is cancelled. The gateway serves
// without it (instances count as healthy until proven otherwise), but then
// only passive detection notices a failed instance.
func (g *Gateway) Run(ctx context.Context) {
	done := make(chan struct{}, len(g.pools))
	for _, pool := range g.pools {
		go func() { pool.Run(ctx); done <- struct{}{} }()
	}
	for range g.pools {
		<-done
	}
}

// New returns the gateway handler for cfg.
//
// Routes are registered on a net/http ServeMux, so matching, wildcards and
// precedence ("the most specific pattern wins") are the standard library's,
// documented and tested there. Conflicting patterns, which ServeMux reports
// by panicking, come back here as an error, so a bad config fails at startup
// rather than taking the process down.
func New(cfg config.Config, version string, logger *slog.Logger) (*Gateway, error) {
	mux := http.NewServeMux()
	// Liveness only: it checks nothing else, so a backend outage never makes
	// Docker restart the gateway.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
	})

	g := &Gateway{}
	pools := map[string]*balancer.Pool{}
	for name, backend := range cfg.Backends {
		pool := balancer.New(name, backend.URLs, balancer.Options{
			HealthCheck: healthCheck(backend.HealthCheck),
			EjectAfter:  backend.EjectAfter,
			EjectFor:    backend.EjectFor,
			Logger:      logger,
		})
		pools[name] = pool
		g.pools = append(g.pools, pool)
	}

	if cfg.RateLimit != nil {
		l, err := newLimiter(cfg.RateLimit, logger)
		if err != nil {
			return nil, err
		}
		g.limiter = l
	}

	// A Config built in code rather than parsed has no body limit set; zero
	// must not mean "reject every body".
	maxBodyBytes := cfg.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = config.DefaultMaxBodyBytes
	}

	// One breaker and one retry budget per backend, shared by its routes.
	base := newTransport()
	breakers := map[string]*resilience.Breaker{}
	transports := map[string]http.RoundTripper{}
	for name, backend := range cfg.Backends {
		if cb := backend.CircuitBreaker; cb != nil {
			breakers[name] = &resilience.Breaker{
				FailureRatio: cb.FailureRatio, MinRequests: cb.MinRequests, OpenFor: cb.OpenFor,
			}
		}
		transports[name] = &retryTransport{
			base: base, pool: pools[name], retries: backend.Retries,
			budget: &resilience.RetryBudget{Ratio: retryBudgetRatio, MinPerSecond: retryBudgetMinPerSecond},
		}
	}

	for _, route := range cfg.Routes {
		backend := cfg.Backends[route.Backend]
		proxy := newProxy(route, backend, pools[route.Backend], transports[route.Backend],
			breakers[route.Backend], maxBodyBytes)
		if g.limiter != nil {
			proxy = g.limiter.wrap(proxy)
		}
		if err := register(mux, route.Match, proxy); err != nil {
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

	g.handler = withRequestID(withAccessLog(mux, logger))
	return g, nil
}

func healthCheck(hc *config.HealthCheck) *balancer.HealthCheck {
	if hc == nil {
		return nil
	}
	return &balancer.HealthCheck{
		Path: hc.Path, Interval: hc.Interval, Timeout: hc.Timeout,
		UnhealthyAfter: hc.UnhealthyAfter, HealthyAfter: hc.HealthyAfter,
	}
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

// newProxy forwards a route's requests to its backend. In order, a request
// meets: the body size limit, the backend's circuit breaker, the retrying
// transport (which picks instances and reports to the pool), and the
// backend's timeout around all of it.
func newProxy(route config.Route, backend config.Backend, pool *balancer.Pool,
	transport http.RoundTripper, breaker *resilience.Breaker, maxBodyBytes int64) http.Handler {
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
			// Joins the instance URL's path with the request's, and sends the
			// instance's own host name as Host.
			pr.SetURL(infoFrom(pr.In.Context()).instance.URL)
			// X-Forwarded-For, -Host and -Proto, so the backend knows who
			// really asked.
			pr.SetXForwarded()
			pr.Out.Header.Set(requestIDHeader, requestID(pr.In.Context()))
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			info := infoFrom(r.Context())
			info.err = err
			var tooLarge *http.MaxBytesError
			switch {
			case errors.As(err, &tooLarge):
				// A chunked body that turned out too big while being sent.
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
					"error":      "request body too large",
					"limit":      tooLarge.Limit,
					"request_id": requestID(r.Context()),
				})
			case errors.Is(r.Context().Err(), context.DeadlineExceeded):
				writeJSON(w, http.StatusGatewayTimeout, map[string]string{
					"error":      fmt.Sprintf("backend %q did not answer within %s", backend.Name, backend.Timeout),
					"request_id": requestID(r.Context()),
				})
			case errors.Is(r.Context().Err(), context.Canceled):
				// The client went away; there is no one to answer. 499 is
				// nginx's convention for this, recorded for the log only.
				info.status = statusClientClosedRequest
			default:
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

		if r.ContentLength > maxBodyBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"error":      "request body too large",
				"limit":      maxBodyBytes,
				"request_id": requestID(r.Context()),
			})
			return
		}
		// Bodies without a Content-Length are cut off at the limit as they
		// are read; the ErrorHandler turns that into a 413.
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

		if breaker != nil {
			allowed, retryAfter := breaker.Allow()
			if !allowed {
				info.breakerOpen = true
				w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(retryAfter.Seconds())))))
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{
					"error":      fmt.Sprintf("backend %q is failing; circuit open", backend.Name),
					"request_id": requestID(r.Context()),
				})
				return
			}
			defer func() { breaker.Record(info.outcome) }()
		}

		info.instance = pool.Pick()
		// The timeout covers the whole exchange with the backend, retries
		// included, and copying its response body to the client.
		ctx, cancel := context.WithTimeout(r.Context(), backend.Timeout)
		defer cancel()
		proxy.ServeHTTP(w, r.WithContext(ctx))
	})
}

// statusClientClosedRequest is nginx's 499: the client disconnected before
// the gateway could answer. It is never sent; it only appears in the log.
const statusClientClosedRequest = 499
