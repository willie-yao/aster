package redact

import "testing"

func TestURLs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "slack webhook in error is stripped",
			in:   `posting to webhook: Post "https://hooks.slack.com/services/T00/B00/secret": dial tcp: timeout`,
			want: `posting to webhook: Post "[redacted-url]": dial tcp: timeout`,
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
