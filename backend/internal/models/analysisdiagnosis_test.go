package models

import "testing"

func TestAnalysisHasUsableDiagnosisSemanticJudgePolicy(t *testing.T) {
	base := AIAnalysis{Disposition: AnalysisDispositionPreliminary, DispositionWarnings: []string{AnalysisWarningSemanticReview}}
	for _, tc := range []struct {
		name string
		mode string
		want bool
	}{
		{name: "advisory", mode: "advisory", want: true},
		{name: "blocking", mode: "blocking"},
		{name: "off", mode: "off"},
		{name: "missing"},
		{name: "unknown", mode: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			analysis := base
			analysis.SemanticJudgeMode = tc.mode
			if got := AnalysisHasUsableDiagnosis(&analysis); got != tc.want {
				t.Fatalf("AnalysisHasUsableDiagnosis() = %t, want %t", got, tc.want)
			}
		})
	}
}
