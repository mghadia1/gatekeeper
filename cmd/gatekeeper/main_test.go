package main

import "testing"

func TestLocalURL(t *testing.T) {
	tests := map[string]string{
		":8080":          "http://127.0.0.1:8080",
		"0.0.0.0:9000":   "http://127.0.0.1:9000",
		"127.0.0.1:7000": "http://127.0.0.1:7000",
		"[::]:8080":      "http://127.0.0.1:8080",
	}
	for listen, want := range tests {
		if got := localURL(listen); got != want {
			t.Errorf("localURL(%q) = %q, want %q", listen, got, want)
		}
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("GATEKEEPER_TEST_VALUE", "set")
	if got := envOr("GATEKEEPER_TEST_VALUE", "fallback"); got != "set" {
		t.Errorf("envOr with the variable set = %q, want %q", got, "set")
	}
	if got := envOr("GATEKEEPER_TEST_UNSET", "fallback"); got != "fallback" {
		t.Errorf("envOr with the variable unset = %q, want %q", got, "fallback")
	}
}
