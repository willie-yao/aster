package tools

import "testing"

func TestEffectiveGrepLimits(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		context, matches     FlexInt
		wantContext, wantMax int
	}{
		{name: "omitted sentinel", context: -1, wantContext: 2, wantMax: 30},
		{name: "explicit zero context", context: 0, wantContext: 0, wantMax: 30},
		{name: "bounded maximums", context: 99, matches: 999, wantContext: 5, wantMax: 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			context, matches := EffectiveGrepLimits(tc.context, tc.matches)
			if context != tc.wantContext || matches != tc.wantMax {
				t.Fatalf("limits=%d/%d, want %d/%d", context, matches, tc.wantContext, tc.wantMax)
			}
		})
	}
}

func TestContentFreePathFilter(t *testing.T) {
	value, supplied, length, redacted := ContentFreePathFilter(" config/*.yaml ")
	if value != "config/*.yaml" || !supplied || length != len("config/*.yaml") || redacted {
		t.Fatalf("safe filter=%q supplied=%t length=%d redacted=%t", value, supplied, length, redacted)
	}
	for _, prose := range []string{"find the secret config please", "find-the-private-failure-please", "find_the_private_failure_please"} {
		value, supplied, length, redacted = ContentFreePathFilter(prose)
		if value != "" || !supplied || length == 0 || !redacted {
			t.Fatalf("prose filter=%q value=%q supplied=%t length=%d redacted=%t", prose, value, supplied, length, redacted)
		}
	}
}
