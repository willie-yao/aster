package patterns

import (
	"testing"

	"github.com/willie-yao/aster/backend/internal/models"
)

// TestRepresentativeAnalyzedFailureAcceptsUsablePreliminary pins that correlation
// consumes a diagnosis whose remaining defects are coverage or remediation
// quality.
func TestRepresentativeAnalyzedFailureAcceptsUsablePreliminary(t *testing.T) {
	failure := func(name, disposition string, warnings ...string) models.TestCase {
		return models.TestCase{
			Name: name, Status: "failed",
			AISummary: &models.AISummary{Summary: "failure"},
			AIAnalysis: &models.AIAnalysis{
				RootCause: "cause", Severity: "High", Mode: "agentic",
				Disposition: disposition, DispositionWarnings: warnings,
			},
		}
	}
	for _, tc := range []struct {
		name     string
		testCase models.TestCase
		want     bool
	}{
		{name: "citations verified", testCase: failure("citations verified", models.AnalysisDispositionCitationsVerified), want: true},
		{
			name:     "preliminary remediation warning",
			testCase: failure("remediation", models.AnalysisDispositionPreliminary, models.AnalysisWarningRemediation),
			want:     true,
		},
		{
			name:     "preliminary grounding warning",
			testCase: failure("grounding", models.AnalysisDispositionPreliminary, models.AnalysisWarningArtifactGrounding),
			want:     true,
		},
		{name: "unstamped", testCase: failure("unstamped", "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &models.BuildResult{TestCases: []models.TestCase{tc.testCase}}
			got := RepresentativeAnalyzedFailure(run)
			if (got != nil) != tc.want {
				t.Fatalf("representative = %v, want usable=%t", got, tc.want)
			}
		})
	}
}

// TestGatherFailuresIncludesPreliminaryDiagnoses proves the correlation input,
// not just the representative selector, carries a usable preliminary diagnosis.
func TestGatherFailuresIncludesPreliminaryDiagnoses(t *testing.T) {
	detail := models.JobDetail{Name: "job", JobID: "job"}
	for _, buildID := range []string{"3", "2", "1"} {
		detail.Runs = append(detail.Runs, models.BuildResult{
			BuildInfo: models.BuildInfo{BuildID: buildID, Result: "FAILURE"},
			TestCases: []models.TestCase{{
				Name: "failed test", Status: "failed",
				AISummary: &models.AISummary{Summary: "failure"},
				AIAnalysis: &models.AIAnalysis{
					RootCause: "shared cause", Severity: "High", Mode: "agentic",
					Disposition:         models.AnalysisDispositionPreliminary,
					DispositionWarnings: []string{models.AnalysisWarningRemediation},
				},
			}},
		})
	}
	failures := GatherFailures(&detail)
	if len(failures) != 3 {
		t.Fatalf("gathered %d failures, want 3 preliminary diagnoses", len(failures))
	}
	if !IsEligible(&detail) {
		t.Fatal("a job whose diagnoses are all preliminary was not eligible for correlation")
	}
}
