package ai

import "testing"

func TestCanonicalizePatternResponseSortsGroupsAndBuilds(t *testing.T) {
	response := canonicalizePatternResponse(patternResponse{
		Groups: []patternCausalGroup{
			{Builds: []string{"d", "c"}, RootCause: " second ", Confidence: "MEDIUM"},
			{Builds: []string{"b", "a"}, RootCause: " first ", Confidence: "HIGH"},
		},
		UnclassifiedBuilds: []string{"f", "e"}, Summary: " summary ",
	})
	if response.Groups[0].Builds[0] != "a" || response.Groups[1].Builds[0] != "c" ||
		response.Groups[0].RootCause != "first" || response.Groups[0].Confidence != "high" ||
		response.UnclassifiedBuilds[0] != "e" || response.Summary != "summary" {
		t.Fatalf("response=%+v", response)
	}
}
