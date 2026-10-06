// Command loadgen sends HTTP requests at a constant rate and reports latency
// percentiles as JSON.
//
//	loadgen -url http://gatekeeper:8080/users/1 -rate 1000 -duration 15s -warmup 3s
//
// It is open-loop: request i is due at start + i/rate whether or not earlier
// requests have finished, and its latency is measured from when it was due,
// not from when it was actually sent. A closed-loop tool (send, wait, send)
// slows down when the server does, so it sends less exactly when latency is
// bad and under-reports the bad latency (coordinated omission). Here a
// server that stalls for a second shows up as a second of latency on every
// request that was due during the stall.
//
// Several -url values (comma-separated) are used in turn, so one run can
// spread across gateway replicas.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

type result struct {
	TargetRate   float64            `json:"target_rate"`
	AchievedRate float64            `json:"achieved_rate"`
	Requests     int                `json:"requests"`
	Statuses     map[string]int     `json:"statuses"`
	Errors       int                `json:"errors"`
	Dropped      int                `json:"dropped"`
	LatencyMs    map[string]float64 `json:"latency_ms"`
}

func main() {
	urls := flag.String("url", "", "target URL, or several separated by commas")
	rate := flag.Float64("rate", 100, "requests per second")
	duration := flag.Duration("duration", 10*time.Second, "how long to measure")
	warmup := flag.Duration("warmup", 2*time.Second, "how long to send before measuring")
	header := flag.String("header", "", `one request header, "Name: value"`)
	maxInFlight := flag.Int("max-in-flight", 10000, "requests outstanding at once before new ones are dropped")
	flag.Parse()
	if *urls == "" || *rate <= 0 {
		flag.Usage()
		os.Exit(2)
	}
	res, err := run(context.Background(), strings.Split(*urls, ","), *rate, *warmup, *duration, *header, *maxInFlight)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(res)
}

type sample struct {
	latency time.Duration
	status  int // 0 for a transport error
}

func run(ctx context.Context, urls []string, rate float64, warmup, duration time.Duration,
	header string, maxInFlight int) (result, error) {
	var headerName, headerValue string
	if header != "" {
		name, value, ok := strings.Cut(header, ":")
		if !ok {
			return result{}, fmt.Errorf("header %q is not Name: value", header)
		}
		headerName, headerValue = strings.TrimSpace(name), strings.TrimSpace(value)
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        maxInFlight,
			MaxIdleConnsPerHost: maxInFlight,
			IdleConnTimeout:     30 * time.Second,
			DisableCompression:  true,
		},
	}

	interval := time.Duration(float64(time.Second) / rate)
	total := int((warmup + duration).Seconds() * rate)
	measureFrom := int(warmup.Seconds() * rate)

	var (
		mu       sync.Mutex
		samples  = make([]sample, 0, total-measureFrom)
		dropped  int
		inFlight = make(chan struct{}, maxInFlight)
		wg       sync.WaitGroup
	)
	start := time.Now()
	for i := range total {
		due := start.Add(time.Duration(i) * interval)
		if wait := time.Until(due); wait > 0 {
			time.Sleep(wait)
		}
		select {
		case inFlight <- struct{}{}:
		default:
			if i >= measureFrom {
				mu.Lock()
				dropped++
				mu.Unlock()
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-inFlight }()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, urls[i%len(urls)], nil)
			if headerName != "" {
				req.Header.Set(headerName, headerValue)
			}
			status := 0
			resp, err := client.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				status = resp.StatusCode
			}
			// Measured from when the request was due, not when it was sent.
			latency := time.Since(due)
			if i >= measureFrom {
				mu.Lock()
				samples = append(samples, sample{latency, status})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start.Add(warmup))
	return summarize(samples, rate, elapsed, dropped), nil
}

func summarize(samples []sample, rate float64, elapsed time.Duration, dropped int) result {
	res := result{
		TargetRate: rate,
		Requests:   len(samples),
		Statuses:   map[string]int{},
		Dropped:    dropped,
		LatencyMs:  map[string]float64{},
	}
	latencies := make([]time.Duration, 0, len(samples))
	for _, s := range samples {
		if s.status == 0 {
			res.Errors++
		} else {
			res.Statuses[fmt.Sprint(s.status)]++
		}
		latencies = append(latencies, s.latency)
	}
	if elapsed > 0 {
		res.AchievedRate = float64(len(samples)) / elapsed.Seconds()
	}
	slices.Sort(latencies)
	for name, q := range map[string]float64{"p50": 0.50, "p90": 0.90, "p99": 0.99, "p999": 0.999, "max": 1} {
		res.LatencyMs[name] = ms(quantile(latencies, q))
	}
	return res
}

// quantile returns the q-quantile of sorted values (nearest rank).
func quantile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(q*float64(len(sorted))+0.5) - 1
	return sorted[max(0, min(rank, len(sorted)-1))]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
