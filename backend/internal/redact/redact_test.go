package redact

import (
	"strings"
	"testing"
)

func TestURLs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "secret-bearing gateway URL is stripped",
			in:   `posting to gateway: Post "https://gateway.example/send?token=secret": dial tcp: timeout`,
			want: `posting to gateway: Post "[redacted-url]": dial tcp: timeout`,
		},
		{
			name: "ai endpoint in url.Error is stripped",
			in:   `post: Post "https://inference.internal.svc:8000/v1/chat/completions": context deadline exceeded`,
			want: `post: Post "[redacted-url]": context deadline exceeded`,
		},
		{
			name: "endpoint with query credential is stripped whole",
			in:   `Get "https://api.example.com/v1?key=sk-secret": connection refused`,
			want: `Get "[redacted-url]": connection refused`,
		},
		{
			name: "no url is unchanged",
			in:   "context deadline exceeded",
			want: "context deadline exceeded",
		},
		{
			name: "multiple urls both stripped",
			in:   "http://a.internal/x and https://b.internal/y failed",
			want: "[redacted-url] and [redacted-url] failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := URLs(tc.in); got != tc.want {
				t.Errorf("URLs(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCredentials(t *testing.T) {
	in := `Authorization: Bearer abc.def token=hidden api_key:also-hidden password=secret {"token":"json-hidden"}`
	got := Credentials(in)
	for _, secret := range []string{"abc.def", "hidden", "also-hidden", "secret", "json-hidden"} {
		if strings.Contains(got, secret) {
			t.Fatalf("Credentials leaked %q: %q", secret, got)
		}
	}
}
