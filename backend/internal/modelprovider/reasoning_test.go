package modelprovider

import "testing"

func TestNormalizeReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  ReasoningEffort
	}{
		{"", ""}, {"  ", ""}, {"none", ReasoningEffortNone}, {" LOW ", ReasoningEffortLow},
		{"Medium", ReasoningEffortMedium}, {"HIGH", ReasoningEffortHigh}, {"xHiGh", ReasoningEffortXHigh}, {" MAX ", ReasoningEffortMax},
	} {
		got, err := NormalizeReasoningEffort(tc.input)
		if err != nil || got != tc.want {
			t.Errorf("NormalizeReasoningEffort(%q) = (%q, %v), want (%q, nil)", tc.input, got, err, tc.want)
		}
	}
}

func TestNormalizeReasoningEffortRejectsUnknown(t *testing.T) {
	got, err := NormalizeReasoningEffort(" ultra ")
	if err == nil || got != "ultra" {
		t.Fatalf("NormalizeReasoningEffort(ultra) = (%q, %v)", got, err)
	}
}
