package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const valid = `{
  "default_timeout": "3s",
  "backends": {
    "users": {"url": "http://users:9000", "timeout": "1s"},
    "orders": {"url": "https://orders.internal"}
  },
  "routes": [
    {"match": "GET /users/{id}", "backend": "users"},
    {"match": "/api/orders/", "backend": "orders", "strip_prefix": "/api"}
  ]
}`

func TestParseValid(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	users, orders := cfg.Backends["users"], cfg.Backends["orders"]
	if len(users.URLs) != 1 || users.URLs[0].String() != "http://users:9000" || users.Timeout != time.Second {
		t.Errorf("users = %+v, want http://users:9000 with a 1s timeout", users)
	}
	if orders.Timeout != 3*time.Second {
		t.Errorf("orders timeout = %v, want the 3s default_timeout", orders.Timeout)
	}
	if len(cfg.Routes) != 2 || cfg.Routes[1] != (Route{"/api/orders/", "orders", "/api"}) {
		t.Errorf("routes = %+v", cfg.Routes)
	}
}

func TestSeveralInstancesHealthChecksAndEjection(t *testing.T) {
	cfg, err := Parse([]byte(`{
	  "backends": {
	    "users": {
	      "urls": ["http://users-1:9000", "http://users-2:9000"],
	      "health_check": {"interval": "2s", "unhealthy_after": 4},
	      "eject_after": 5, "eject_for": "30s"
	    },
	    "plain": {"url": "http://plain"}
	  },
	  "routes": [{"match": "/", "backend": "users"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	users := cfg.Backends["users"]
	if len(users.URLs) != 2 || users.URLs[1].Host != "users-2:9000" {
		t.Errorf("urls = %v", users.URLs)
	}
	want := HealthCheck{Path: "/healthz", Interval: 2 * time.Second, Timeout: time.Second, UnhealthyAfter: 4, HealthyAfter: 2}
	if users.HealthCheck == nil || *users.HealthCheck != want {
		t.Errorf("health check = %+v, want %+v (defaults filled in)", users.HealthCheck, want)
	}
	if users.EjectAfter != 5 || users.EjectFor != 30*time.Second {
		t.Errorf("ejection = %d / %v, want 5 / 30s", users.EjectAfter, users.EjectFor)
	}
	plain := cfg.Backends["plain"]
	if plain.HealthCheck != nil || plain.EjectAfter != DefaultEjectAfter || plain.EjectFor != DefaultEjectFor {
		t.Errorf("plain backend = %+v, want no health check and default ejection", plain)
	}
}

func TestBuiltInDefaultTimeout(t *testing.T) {
	cfg, err := Parse([]byte(`{"backends": {"a": {"url": "http://a"}}, "routes": [{"match": "/", "backend": "a"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Backends["a"].Timeout; got != DefaultTimeout {
		t.Fatalf("timeout = %v, want %v", got, DefaultTimeout)
	}
}

func TestParseRejects(t *testing.T) {
	tests := map[string]struct {
		json, wantInError string
	}{
		"unknown field": {
			`{"backends": {}, "routes": [], "rotues": []}`, "unknown field",
		},
		"no routes": {
			`{"backends": {"a": {"url": "http://a"}}, "routes": []}`, "at least one route",
		},
		"unknown backend": {
			`{"backends": {"a": {"url": "http://a"}}, "routes": [{"match": "/", "backend": "b"}]}`, `unknown backend "b"`,
		},
		"non-http scheme": {
			`{"backends": {"a": {"url": "ftp://a"}}, "routes": [{"match": "/", "backend": "a"}]}`, "must be http or https",
		},
		"no host": {
			`{"backends": {"a": {"url": "http://"}}, "routes": [{"match": "/", "backend": "a"}]}`, "has no host",
		},
		"bad timeout": {
			`{"backends": {"a": {"url": "http://a", "timeout": "soon"}}, "routes": [{"match": "/", "backend": "a"}]}`, "timeout",
		},
		"zero timeout": {
			`{"backends": {"a": {"url": "http://a", "timeout": "0s"}}, "routes": [{"match": "/", "backend": "a"}]}`, "must be positive",
		},
		"url and urls": {
			`{"backends": {"a": {"url": "http://a", "urls": ["http://b"]}}, "routes": [{"match": "/", "backend": "a"}]}`, "not both",
		},
		"no url": {
			`{"backends": {"a": {}}, "routes": [{"match": "/", "backend": "a"}]}`, "url or urls is required",
		},
		"bad url in list": {
			`{"backends": {"a": {"urls": ["http://ok", "gopher://no"]}}, "routes": [{"match": "/", "backend": "a"}]}`, "must be http or https",
		},
		"negative eject_after": {
			`{"backends": {"a": {"url": "http://a", "eject_after": -1}}, "routes": [{"match": "/", "backend": "a"}]}`, "eject_after",
		},
		"relative health path": {
			`{"backends": {"a": {"url": "http://a", "health_check": {"path": "healthz"}}}, "routes": [{"match": "/", "backend": "a"}]}`, "health_check.path",
		},
		"bad health interval": {
			`{"backends": {"a": {"url": "http://a", "health_check": {"interval": "often"}}}, "routes": [{"match": "/", "backend": "a"}]}`, "health_check.interval",
		},
		"empty match": {
			`{"backends": {"a": {"url": "http://a"}}, "routes": [{"match": " ", "backend": "a"}]}`, "match is empty",
		},
		"relative strip_prefix": {
			`{"backends": {"a": {"url": "http://a"}}, "routes": [{"match": "/", "backend": "a", "strip_prefix": "api"}]}`, "strip_prefix",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tt.json))
			if err == nil || !strings.Contains(err.Error(), tt.wantInError) {
				t.Fatalf("error = %v, want one containing %q", err, tt.wantInError)
			}
		})
	}
}

func TestParseReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte(`{
	  "backends": {"a": {"url": "ftp://a"}, "b": {"url": "http://b", "timeout": "-1s"}},
	  "routes": [{"match": "/", "backend": "missing"}]
	}`))
	for _, want := range []string{"must be http or https", "must be positive", `unknown backend "missing"`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not report %q", err, want)
		}
	}
}

func TestLoadNamesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gatekeeper.json")
	if err := os.WriteFile(path, []byte(`{"routes": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %v, want it to name %s", err, path)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("Load of a missing file returned no error")
	}
}
