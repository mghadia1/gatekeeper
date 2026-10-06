// Package config loads and validates the gateway's configuration file.
//
// The file is JSON. Unknown fields are rejected, so a typo is an error at
// startup rather than a setting silently ignored in production.
//
//	{
//	  "default_timeout": "10s",
//	  "backends": {
//	    "users": {
//	      "urls": ["http://users-1:9000", "http://users-2:9000"],
//	      "timeout": "2s",
//	      "health_check": {"path": "/healthz", "interval": "2s"},
//	      "eject_after": 3, "eject_for": "10s"
//	    },
//	    "orders": {"url": "http://orders:9000"}
//	  },
//	  "routes": [
//	    {"match": "GET /users/{id}", "backend": "users"},
//	    {"match": "/api/orders/", "backend": "orders", "strip_prefix": "/api"}
//	  ]
//	}
//
// A route's match is a net/http ServeMux pattern: an optional method, then a
// path that may contain {name} wildcards, end in {name...}, or end in "/" to
// match everything below it.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Config is the validated configuration.
type Config struct {
	Routes   []Route
	Backends map[string]Backend
}

// Route sends requests matching Match to a backend.
type Route struct {
	Match       string // a net/http ServeMux pattern
	Backend     string // a key of Config.Backends
	StripPrefix string // removed from the path before forwarding, if set
}

// Backend is a service the gateway forwards to, with one or more instances.
type Backend struct {
	Name    string
	URLs    []*url.URL
	Timeout time.Duration // the whole round trip to the backend

	// HealthCheck is nil when the backend has no active health checks.
	HealthCheck *HealthCheck
	// Passive detection: after EjectAfter consecutive failed requests, an
	// instance is taken out of rotation for EjectFor.
	EjectAfter int
	EjectFor   time.Duration
}

// HealthCheck configures active checks of each instance.
type HealthCheck struct {
	Path           string
	Interval       time.Duration
	Timeout        time.Duration
	UnhealthyAfter int // consecutive failed checks before marking down
	HealthyAfter   int // consecutive passing checks before marking up
}

// Defaults for passive detection and for health_check fields left out.
const (
	DefaultEjectAfter     = 3
	DefaultEjectFor       = 10 * time.Second
	DefaultCheckPath      = "/healthz"
	DefaultCheckInterval  = 5 * time.Second
	DefaultCheckTimeout   = time.Second
	DefaultUnhealthyAfter = 2
	DefaultHealthyAfter   = 2
)

type file struct {
	DefaultTimeout string                 `json:"default_timeout"`
	Backends       map[string]fileBackend `json:"backends"`
	Routes         []fileRoute            `json:"routes"`
}

type fileBackend struct {
	URL         string           `json:"url"`
	URLs        []string         `json:"urls"`
	Timeout     string           `json:"timeout"`
	HealthCheck *fileHealthCheck `json:"health_check"`
	EjectAfter  int              `json:"eject_after"`
	EjectFor    string           `json:"eject_for"`
}

type fileHealthCheck struct {
	Path           string `json:"path"`
	Interval       string `json:"interval"`
	Timeout        string `json:"timeout"`
	UnhealthyAfter int    `json:"unhealthy_after"`
	HealthyAfter   int    `json:"healthy_after"`
}

type fileRoute struct {
	Match       string `json:"match"`
	Backend     string `json:"backend"`
	StripPrefix string `json:"strip_prefix"`
}

// DefaultTimeout applies to backends that set none, when the file sets no
// default_timeout either.
const DefaultTimeout = 10 * time.Second

// Load reads and validates the configuration file at path.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse validates a configuration given as JSON. Once the JSON decodes,
// every problem found is reported, not only the first. A syntax error or an
// unknown field stops decoding, so it is reported alone.
func Parse(data []byte) (Config, error) {
	var f file
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Config{}, fmt.Errorf("invalid JSON: %w", err)
	}

	var problems []error
	fallback := DefaultTimeout
	if f.DefaultTimeout != "" {
		d, err := parsePositiveDuration(f.DefaultTimeout)
		if err != nil {
			problems = append(problems, fmt.Errorf("default_timeout: %w", err))
		} else {
			fallback = d
		}
	}

	cfg := Config{Backends: map[string]Backend{}}
	names := make([]string, 0, len(f.Backends))
	for name := range f.Backends {
		names = append(names, name)
	}
	sort.Strings(names) // report problems in a stable order
	for _, name := range names {
		b := f.Backends[name]
		backend, errs := parseBackend(name, b, fallback)
		problems = append(problems, errs...)
		cfg.Backends[name] = backend
	}

	if len(f.Routes) == 0 {
		problems = append(problems, errors.New("routes: at least one route is required"))
	}
	for i, r := range f.Routes {
		where := fmt.Sprintf("route %d (%q)", i, r.Match)
		if strings.TrimSpace(r.Match) == "" {
			problems = append(problems, fmt.Errorf("route %d: match is empty", i))
		}
		if _, ok := f.Backends[r.Backend]; !ok {
			problems = append(problems, fmt.Errorf("%s: unknown backend %q", where, r.Backend))
		}
		if r.StripPrefix != "" && !strings.HasPrefix(r.StripPrefix, "/") {
			problems = append(problems, fmt.Errorf("%s: strip_prefix must start with /", where))
		}
		cfg.Routes = append(cfg.Routes, Route{Match: r.Match, Backend: r.Backend, StripPrefix: r.StripPrefix})
	}

	if err := errors.Join(problems...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseBackend(name string, b fileBackend, fallbackTimeout time.Duration) (Backend, []error) {
	var problems []error
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf("backend %q: "+format, append([]any{name}, args...)...))
	}
	backend := Backend{
		Name: name, Timeout: fallbackTimeout,
		EjectAfter: DefaultEjectAfter, EjectFor: DefaultEjectFor,
	}

	raw := b.URLs
	switch {
	case b.URL != "" && len(b.URLs) > 0:
		fail("set url or urls, not both")
	case b.URL != "":
		raw = []string{b.URL}
	case len(b.URLs) == 0:
		fail("url or urls is required")
	}
	for _, r := range raw {
		u, err := url.Parse(r)
		switch {
		case err != nil:
			fail("url: %v", err)
		case u.Scheme != "http" && u.Scheme != "https":
			fail("url %q must be http or https", r)
		case u.Host == "":
			fail("url %q has no host", r)
		default:
			backend.URLs = append(backend.URLs, u)
		}
	}

	duration := func(field, value string, into *time.Duration) {
		if value == "" {
			return
		}
		d, err := parsePositiveDuration(value)
		if err != nil {
			fail("%s: %v", field, err)
			return
		}
		*into = d
	}
	positive := func(field string, value int, into *int) {
		switch {
		case value < 0:
			fail("%s must be at least 1", field)
		case value > 0:
			*into = value
		}
	}

	duration("timeout", b.Timeout, &backend.Timeout)
	positive("eject_after", b.EjectAfter, &backend.EjectAfter)
	duration("eject_for", b.EjectFor, &backend.EjectFor)

	if hc := b.HealthCheck; hc != nil {
		check := &HealthCheck{
			Path: DefaultCheckPath, Interval: DefaultCheckInterval, Timeout: DefaultCheckTimeout,
			UnhealthyAfter: DefaultUnhealthyAfter, HealthyAfter: DefaultHealthyAfter,
		}
		if hc.Path != "" {
			if !strings.HasPrefix(hc.Path, "/") {
				fail("health_check.path must start with /")
			}
			check.Path = hc.Path
		}
		duration("health_check.interval", hc.Interval, &check.Interval)
		duration("health_check.timeout", hc.Timeout, &check.Timeout)
		positive("health_check.unhealthy_after", hc.UnhealthyAfter, &check.UnhealthyAfter)
		positive("health_check.healthy_after", hc.HealthyAfter, &check.HealthyAfter)
		backend.HealthCheck = check
	}
	return backend, problems
}

func parsePositiveDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q must be positive", s)
	}
	return d, nil
}
