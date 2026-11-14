package server

import (
	"net/url"
	"strings"
	"testing"
)

func TestMaskPresignedQuery(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"list-type=2&prefix=a%2Fb", "list-type=2&prefix=a%2Fb"},
		{
			"X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKID%2F20260101%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Signature=deadbeef",
			"X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=***&X-Amz-Signature=***",
		},
		// Percent-encoded and case-varied names authenticate the same way, so
		// they must be masked the same way.
		{"X%2DAmz%2DSignature=deadbeef&x-amz-credential=AKID%2Fx", "X%2DAmz%2DSignature=***&x-amz-credential=***"},
		{"%58-Amz-Security-Token=tok&a=1", "%58-Amz-Security-Token=***&a=1"},
		// Unparseable queries are masked whole.
		{"X-Amz-Signature=dead%zzbeef", "***"},
		{"a=1;X-Amz-Signature=deadbeef", "***"},
	} {
		if got := maskPresignedQuery(c.in); got != c.want {
			t.Errorf("maskPresignedQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Whatever the encoding, no value of a sensitive parameter survives masking.
func TestMaskPresignedQueryNeverLeaksSecrets(t *testing.T) {
	names := []string{"X-Amz-Signature", "x-amz-signature", "X%2DAmz%2DSignature", "X-Amz-Credential", "X%2damz%2dcredential", "X-Amz-Security-Token", "X-Amz-Security%2DToken"}
	for _, n := range names {
		raw := "foo=bar&" + n + "=SECRETVALUE"
		vals, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(vals) != 2 {
			t.Fatalf("%s: unexpected parse %v", n, vals)
		}
		if got := maskPresignedQuery(raw); strings.Contains(got, "SECRETVALUE") {
			t.Errorf("%s leaked: %q", n, got)
		}
	}
}
