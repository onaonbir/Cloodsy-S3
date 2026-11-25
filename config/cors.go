package config

import (
	"fmt"
	"strings"
)

// OriginAllowed reports whether a browser Origin matches one of the
// configured cors_origins entries. An entry is "*" (any origin), an exact
// origin such as "https://app.example.com" (compared case-insensitively), or
// a subdomain wildcard such as "https://*.example.com": the "*" stands for
// one or more subdomain labels, so it matches "https://a.example.com" and
// "https://a.b.example.com" but not "https://example.com" itself. Scheme and
// port must match exactly ("https://*.example.com" does not match
// "https://a.example.com:8443").
func OriginAllowed(patterns []string, origin string) bool {
	if origin == "" {
		return false
	}
	for _, p := range patterns {
		if p == "*" || strings.EqualFold(p, origin) {
			return true
		}
		if matchWildcardOrigin(p, origin) {
			return true
		}
	}
	return false
}

func matchWildcardOrigin(pattern, origin string) bool {
	scheme, rest, ok := strings.Cut(pattern, "://*.")
	if !ok || rest == "" {
		return false
	}
	prefix := scheme + "://"
	if len(origin) <= len(prefix) || !strings.EqualFold(origin[:len(prefix)], prefix) {
		return false
	}
	host := origin[len(prefix):]
	suffix := "." + rest
	if len(host) <= len(suffix) || !strings.EqualFold(host[len(host)-len(suffix):], suffix) {
		return false
	}
	// What the "*" covers must be hostname labels only: no port, path,
	// userinfo or empty label can slip in through it.
	sub := host[:len(host)-len(suffix)]
	for _, label := range strings.Split(sub, ".") {
		if label == "" {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// validateCORSOrigins rejects wildcard entries OriginAllowed would never
// match, so a typo fails at start-up instead of silently blocking browsers.
// Entries without "*" are left as they are (compared literally).
func validateCORSOrigins(name string, origins []string) error {
	for _, o := range origins {
		if o == "*" || !strings.Contains(o, "*") {
			continue
		}
		scheme, rest, ok := strings.Cut(o, "://*.")
		if !ok || (scheme != "http" && scheme != "https") || rest == "" ||
			strings.ContainsAny(rest, "*/") || strings.HasPrefix(rest, ".") || strings.HasSuffix(rest, ".") {
			return fmt.Errorf("%s: invalid wildcard origin %q (use \"*\" or a subdomain wildcard such as \"https://*.example.com\")", name, o)
		}
	}
	return nil
}
