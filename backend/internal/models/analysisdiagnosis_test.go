package models

import "testing"

func TestAnalysisHasUsableDiagnosisAcceptsCitationsVerifiedAndPreliminary(t *testing.T) {
	for _, tc := range []struct {
		name        string
		disposition string
		want        bool
	}{
		{name: "citations verified", disposition: AnalysisDispositionCitationsVerified, want: true},
		{name: "preliminary", disposition: AnalysisDispositionPreliminary, want: true},
		{name: "unstamped"},
		{name: "unknown", disposition: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := AnalysisHasUsableDiagnosis(&AIAnalysis{Disposition: tc.disposition}); got != tc.want {
				t.Fatalf("AnalysisHasUsableDiagnosis() = %t, want %t", got, tc.want)
			}
		})
	}
	if AnalysisHasUsableDiagnosis(nil) {
		t.Fatal("nil analysis was usable")
	}
}
