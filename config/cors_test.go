package config

import (
	"strings"
	"testing"
)

func TestOriginAllowed(t *testing.T) {
	patterns := []string{"https://app.example", "http://*.apps.test", "http://*.dev.test:8080"}
	cases := []struct {
		origin string
		want   bool
	}{
		{"https://app.example", true},
		{"HTTPS://APP.EXAMPLE", true},
		{"https://other.example", false},
		{"http://team.apps.test", true},
		{"http://a.b.apps.test", true},
		{"HTTP://Acme.Apps.Test", true},
		{"http://apps.test", false},           // the apex needs its own entry
		{"https://team.apps.test", false},     // scheme must match
		{"http://team.apps.test:9418", false}, // port must match
		{"http://evilapps.test", false},
		{"http://team.apps.test.evil.com", false},
		{"http://.apps.test", false},
		{"http://a..apps.test", false},
		{"http://a_b.apps.test", false},
		{"http://user@x.apps.test", false},
		{"http://x/y.apps.test", false},
		{"http://a.dev.test:8080", true},
		{"http://a.dev.test", false},
		{"", false},
	}
	for _, c := range cases {
		if got := OriginAllowed(patterns, c.origin); got != c.want {
			t.Errorf("OriginAllowed(%q) = %v, want %v", c.origin, got, c.want)
		}
	}
	if !OriginAllowed([]string{"*"}, "http://anything.example") {
		t.Error(`"*" should allow any origin`)
	}
	if OriginAllowed(nil, "http://a.example") {
		t.Error("no patterns should allow nothing")
	}
}

func TestValidateCORSOrigins(t *testing.T) {
	ok := []string{"*", "https://app.example", "http://*.apps.test", "https://*.example.com:8443", "https://app.example/"}
	if err := validateCORSOrigins("server.cors_origins", ok); err != nil {
		t.Fatalf("valid origins rejected: %v", err)
	}
	for _, bad := range []string{"*.example.com", "https://*example.com", "https://a.*.example.com", "ftp://*.example.com", "https://*.", "https://*.example.com/x", "https://*.*.example.com"} {
		err := validateCORSOrigins("server.cors_origins", []string{bad})
		if err == nil || !strings.Contains(err.Error(), "server.cors_origins") {
			t.Errorf("%q: got %v, want an error", bad, err)
		}
	}
}
