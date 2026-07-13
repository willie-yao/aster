package main

import "testing"

func TestTrustedOrigins_DerivesRedirectHost(t *testing.T) {
	got := trustedOrigins("https://dash.example.net/api/auth/callback", "https://alt.example, other.example")
	want := map[string]bool{"dash.example.net": true, "https://alt.example": true, "other.example": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want keys %v", got, want)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected origin %q in %v", g, got)
		}
	}
}

func TestTrustedOrigins_EmptyRedirect(t *testing.T) {
	if got := trustedOrigins("", ""); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
	got := trustedOrigins("", "only.example")
	if len(got) != 1 || got[0] != "only.example" {
		t.Errorf("got %v, want [only.example]", got)
	}
}
