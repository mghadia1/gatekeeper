package gateway

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"

	"github.com/mghadia1/gatekeeper/internal/balancer"
	"github.com/mghadia1/gatekeeper/internal/resilience"
)

// The retry budget: retries may add at most 20% to a backend's traffic over
// the last ten seconds, plus 3 per second so a quiet backend can retry too.
const (
	retryBudgetRatio        = 0.2
	retryBudgetMinPerSecond = 3
)

// retryTransport sends a request to the instance the handler picked and, if
// that instance fails in a way that is safe to retry, to another one. It
// reports every attempt to the pool, so passive detection sees each
// failure, and records the final outcome for the circuit breaker.
//
// What counts as the instance failing: a connection error, a timeout, or a
// 503. A request is retried only if it has no body (a body cannot be sent
// twice) and either its method is safe (GET, HEAD, OPTIONS) or the
// connection never opened, so the backend cannot have seen it. Retries also
// need room in the retry budget and in the backend's timeout, which covers
// all attempts together.
type retryTransport struct {
	base    http.RoundTripper
	pool    *balancer.Pool
	retries int
	budget  *resilience.RetryBudget
	backend string
	metrics *metrics
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	info := infoFrom(ctx)
	t.budget.Request()
	for attempt := 1; ; attempt++ {
		info.attempts = attempt
		instance := info.instance
		resp, err := t.base.RoundTrip(req)

		switch judge(ctx, resp, err) {
		case answered:
			t.pool.ReportSuccess(instance)
			t.metrics.attempts.WithLabelValues(t.backend, "success").Inc()
			info.outcome = resilience.Success
			return resp, nil
		case notTheBackend:
			info.outcome = resilience.Ignored
			return resp, err
		}

		t.pool.ReportFailure(instance)
		t.metrics.attempts.WithLabelValues(t.backend, "failure").Inc()
		info.outcome = resilience.Failure
		if attempt > t.retries || ctx.Err() != nil || !retryable(req, resp, err) || !t.budget.TryRetry() {
			return resp, err
		}
		if resp != nil {
			// Drain a little so the connection can be reused, then let go.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}
		t.metrics.retries.WithLabelValues(t.backend).Inc()
		next := t.pool.PickExcept(instance)
		info.instance = next
		req = req.Clone(ctx)
		req.URL.Scheme, req.URL.Host = next.URL.Scheme, next.URL.Host
	}
}

type verdict int

const (
	answered      verdict = iota // a response other than 503
	failed                       // the instance failed: connection, timeout, 503
	notTheBackend                // the client went away, or its body was too big
)

func judge(ctx context.Context, resp *http.Response, err error) verdict {
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || errors.Is(ctx.Err(), context.Canceled) {
			return notTheBackend
		}
		return failed
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		return failed
	}
	return answered
}

func retryable(req *http.Request, resp *http.Response, err error) bool {
	if req.Body != nil && req.Body != http.NoBody {
		return false
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	// Any other method only if the connection was never made.
	var opErr *net.OpError
	return resp == nil && errors.As(err, &opErr) && opErr.Op == "dial"
}
