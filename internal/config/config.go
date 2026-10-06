// Package config loads and validates the gateway's configuration file.
//
// The file is JSON. Unknown fields are rejected, so a typo is an error at
// startup rather than a setting silently ignored in production.
//
//	{
//	  "default_timeout": "10s",
//	  "backends": {
//	    "users": {"url": "http://users:9000", "timeout": "2s"}
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

// Backend is a service the gateway forwards to.
type Backend struct {
	Name    string
	URL     *url.URL
	Timeout time.Duration // the whole round trip to the backend
}

type file struct {
	DefaultTimeout string                 `json:"default_timeout"`
	Backends       map[string]fileBackend `json:"backends"`
	Routes         []fileRoute            `json:"routes"`
}

type fileBackend struct {
	URL     string `json:"url"`
	Timeout string `json:"timeout"`
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
		backend := Backend{Name: name, Timeout: fallback}
		u, err := url.Parse(b.URL)
		switch {
		case err != nil:
			problems = append(problems, fmt.Errorf("backend %q: url: %w", name, err))
		case u.Scheme != "http" && u.Scheme != "https":
			problems = append(problems, fmt.Errorf("backend %q: url %q must be http or https", name, b.URL))
		case u.Host == "":
			problems = append(problems, fmt.Errorf("backend %q: url %q has no host", name, b.URL))
		default:
			backend.URL = u
		}
		if b.Timeout != "" {
			d, err := parsePositiveDuration(b.Timeout)
			if err != nil {
				problems = append(problems, fmt.Errorf("backend %q: timeout: %w", name, err))
			} else {
				backend.Timeout = d
			}
		}
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
