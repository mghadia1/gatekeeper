package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mghadia1/gatekeeper/internal/balancer"
	"github.com/mghadia1/gatekeeper/internal/resilience"
)

const requestIDHeader = "X-Request-ID"

// A caller's request id is kept only if it is short and plain, so it cannot
// inject anything into logs or headers.
var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type contextKey int

const (
	requestIDKey contextKey = iota
	infoKey
)

// requestInfo is filled in as a request passes through the gateway and read
// by the access log when it completes.
type requestInfo struct {
	route       string
	backend     string
	instance    *balancer.Instance
	client      string // the rate limit key: a hashed API key or an IP
	rateLimited bool
	attempts    int                // tries at the backend, retries included
	outcome     resilience.Outcome // for the circuit breaker
	breakerOpen bool
	status      int // overrides the written status (only for 499)
	err         error
}

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func infoFrom(ctx context.Context) *requestInfo {
	if info, ok := ctx.Value(infoKey).(*requestInfo); ok {
		return info
	}
	return &requestInfo{} // outside the middleware (in a unit test, say)
}

// withRequestID gives every request an id: the caller's X-Request-ID if it
// is safe, otherwise a new random one. The id is forwarded to the backend
// and returned to the client.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !safeRequestID.MatchString(id) {
			id = newRequestID()
		}
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read does not fail on supported platforms
	return hex.EncodeToString(b)
}

// withAccessLog writes one log line per request when it completes. It must
// sit inside withRequestID, so the id is known.
func withAccessLog(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := &requestInfo{}
		rec := &recorder{ResponseWriter: w, requestID: requestID(r.Context())}
		started := time.Now()
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), infoKey, info)))

		status := rec.status
		if status == 0 {
			status = http.StatusOK // the handler wrote nothing at all
		}
		if info.status != 0 {
			status = info.status
		}
		level := slog.LevelInfo
		if strings.HasPrefix(r.URL.Path, "/healthz") {
			level = slog.LevelDebug // polled by Docker every few seconds
		}
		attrs := []slog.Attr{
			slog.String("request_id", rec.requestID),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path), // no query string: it may hold secrets
			slog.Int("status", status),
			slog.Float64("duration_ms", float64(time.Since(started).Microseconds())/1000),
			slog.Int64("bytes", rec.bytes),
		}
		if info.route != "" {
			attrs = append(attrs, slog.String("route", info.route), slog.String("backend", info.backend))
		}
		if info.instance != nil {
			attrs = append(attrs, slog.String("instance", info.instance.URL.Host))
		}
		if info.client != "" {
			attrs = append(attrs, slog.String("client", info.client))
		}
		if info.rateLimited {
			attrs = append(attrs, slog.Bool("rate_limited", true))
		}
		if info.attempts > 1 {
			attrs = append(attrs, slog.Int("attempts", info.attempts))
		}
		if info.breakerOpen {
			attrs = append(attrs, slog.Bool("circuit_open", true))
		}
		if info.err != nil {
			attrs = append(attrs, slog.String("error", info.err.Error()))
		}
		logger.LogAttrs(r.Context(), level, "request", attrs...)
	})
}

// recorder notes the status and size of a response, and sets X-Request-ID
// as the headers go out. Setting it then, rather than up front, means a
// backend that echoes the header does not produce two copies of it: the
// proxy copies the backend's headers in first, and this replaces them.
type recorder struct {
	http.ResponseWriter
	requestID string
	status    int
	bytes     int64
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
		r.Header().Set(requestIDHeader, r.requestID)
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer, which the
// reverse proxy uses to flush streamed responses.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
