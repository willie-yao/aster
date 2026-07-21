package orka

import (
	"strings"
	"testing"
)

func TestNormalizeAPIMode(t *testing.T) {
	for input, want := range map[string]string{
		"":                 APIModeAuto,
		" AUTO ":           APIModeAuto,
		"responses":        APIModeResponses,
		"CHAT_COMPLETIONS": APIModeChatCompletions,
	} {
		got, err := NormalizeAPIMode(input)
		if err != nil {
			t.Fatalf("NormalizeAPIMode(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("NormalizeAPIMode(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := NormalizeAPIMode("chat"); err == nil {
		t.Fatal("NormalizeAPIMode accepted an unsupported mode")
	}
}

func TestValidateObservedAPIMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expected string
		observed string
		wantErr  string
	}{
		{name: "auto responses", expected: APIModeAuto, observed: APIModeResponses},
		{name: "auto chat", expected: APIModeAuto, observed: APIModeChatCompletions},
		{name: "responses", expected: APIModeResponses, observed: APIModeResponses},
		{name: "mismatch", expected: APIModeResponses, observed: APIModeChatCompletions, wantErr: "expected responses"},
		{name: "missing", expected: APIModeAuto, wantErr: "did not report"},
		{name: "mixed", expected: APIModeAuto, observed: "mixed", wantErr: "multiple API modes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateObservedAPIMode(tc.expected, tc.observed)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("ValidateObservedAPIMode: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("ValidateObservedAPIMode error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
