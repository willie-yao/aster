package patterns

import (
	"testing"

	"github.com/willie-yao/aster/backend/internal/aggregator"
	"github.com/willie-yao/aster/backend/internal/models"
)

type failedBuild struct {
	buildID  string
	testName string
	message  string
}

func signatureJob(jobID string, builds ...failedBuild) models.JobDetail {
	detail := models.JobDetail{Name: jobID, JobID: jobID}
	for _, build := range builds {
		detail.Runs = append(detail.Runs, models.BuildResult{
			BuildInfo: models.BuildInfo{BuildID: build.buildID, Result: "FAILURE", Passed: false},
			TestCases: []models.TestCase{{
				Name: build.testName, Status: "failed", FailureMessage: build.message,
				AISummary:  &models.AISummary{Summary: "failure"},
				AIAnalysis: &models.AIAnalysis{RootCause: "cause", Severity: "High", Mode: "agentic", Disposition: models.AnalysisDispositionGrounded},
			}},
		})
	}
	return detail
}

func groupOf(buildIDs ...string) models.PatternCausalGroup {
	return models.PatternCausalGroup{Builds: buildIDs, RootCause: "cause", Confidence: "high"}
}

// The signature has to survive what genuinely varies between passes, which is the
// model's wording, which builds are in the window, and per-build timestamps.
func TestCausalGroupSignatureIgnoresProseAndTimestamps(t *testing.T) {
	first := signatureJob("job-1",
		failedBuild{"10", "TestReconcile", "context deadline exceeded at 2026-01-01T00:00:00Z"},
		failedBuild{"11", "TestReconcile", "context deadline exceeded at 2026-01-02T11:12:13Z"},
	)
	want := CausalGroupSignature(first, groupOf("10", "11"))
	if want == "" {
		t.Fatal("no signature derived from analyzed failures")
	}

	// Same cause, different builds, different timestamps.
	later := signatureJob("job-1",
		failedBuild{"90", "TestReconcile", "context deadline exceeded at 2026-06-09T04:05:06Z"},
	)
	if got := CausalGroupSignature(later, groupOf("90")); got != want {
		t.Fatalf("signature changed across the window: %q != %q", got, want)
	}

	// A different job with the same failure is a different cause.
	other := signatureJob("job-2", failedBuild{"10", "TestReconcile", "context deadline exceeded at 2026-01-01T00:00:00Z"})
	if got := CausalGroupSignature(other, groupOf("10")); got == want {
		t.Fatal("two jobs collapsed into one signature")
	}

	// A different test with the same message is a different cause.
	renamed := signatureJob("job-1", failedBuild{"10", "TestOther", "context deadline exceeded at 2026-01-01T00:00:00Z"})
	if got := CausalGroupSignature(renamed, groupOf("10")); got == want {
		t.Fatal("two tests collapsed into one signature")
	}

	// A genuinely different failure is a different cause.
	different := signatureJob("job-1", failedBuild{"10", "TestReconcile", "connection refused"})
	if got := CausalGroupSignature(different, groupOf("10")); got == want {
		t.Fatal("a different error produced the same signature")
	}
}

func TestCausalGroupSignatureUsesTheDominantFailure(t *testing.T) {
	detail := signatureJob("job-1",
		failedBuild{"10", "TestReconcile", "timed out"},
		failedBuild{"11", "TestReconcile", "timed out"},
		failedBuild{"12", "TestUnrelated", "connection refused"},
	)
	dominant := CausalGroupSignature(signatureJob("job-1", failedBuild{"10", "TestReconcile", "timed out"}), groupOf("10"))
	if got := CausalGroupSignature(detail, groupOf("10", "11", "12")); got != dominant {
		t.Fatalf("signature=%q, want the dominant failure's signature %q", got, dominant)
	}
	// Reordering the builds must not change the answer.
	if got := CausalGroupSignature(detail, groupOf("12", "11", "10")); got != dominant {
		t.Fatalf("build order changed the signature: %q != %q", got, dominant)
	}
}

// A tie has no dominant failure, so the choice must still be deterministic
// rather than depending on map iteration order.
func TestCausalGroupSignatureBreaksTiesDeterministically(t *testing.T) {
	detail := signatureJob("job-1",
		failedBuild{"10", "TestAlpha", "boom"},
		failedBuild{"11", "TestBeta", "boom"},
	)
	first := CausalGroupSignature(detail, groupOf("10", "11"))
	if first == "" {
		t.Fatal("a tie produced no signature")
	}
	for range 20 {
		if got := CausalGroupSignature(detail, groupOf("11", "10")); got != first {
			t.Fatalf("tie-break was unstable: %q != %q", got, first)
		}
	}
	alpha := CausalGroupSignature(signatureJob("job-1", failedBuild{"10", "TestAlpha", "boom"}), groupOf("10"))
	if first != alpha {
		t.Fatalf("tie did not resolve to the lexicographically first failure: %q != %q", first, alpha)
	}
}

func TestCausalGroupSignatureIsEmptyWithoutResolvableFailures(t *testing.T) {
	detail := signatureJob("job-1", failedBuild{"10", "TestReconcile", "timed out"})
	if got := CausalGroupSignature(detail, groupOf("999")); got != "" {
		t.Fatalf("signature=%q for builds outside the window, want empty", got)
	}
	unanalyzed := models.JobDetail{Name: "job-1", JobID: "job-1", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "10", Result: "FAILURE"},
		TestCases: []models.TestCase{{Name: "TestReconcile", Status: "failed"}},
	}}}
	if got := CausalGroupSignature(unanalyzed, groupOf("10")); got != "" {
		t.Fatalf("signature=%q without an analyzed failure, want empty", got)
	}
	if got := CausalGroupSignature(models.JobDetail{}, groupOf("10")); got != "" {
		t.Fatalf("signature=%q without a job identity, want empty", got)
	}
}

// A failure with no message carries no evidence to key on. Keying anyway would
// collapse every messageless failure in a test into one cause and let a verdict
// for one be reused for another.
func TestCausalGroupSignatureRejectsFailuresWithNoMessage(t *testing.T) {
	for _, message := range []string{"", "   ", "\n\t "} {
		detail := signatureJob("job-1", failedBuild{"10", "TestReconcile", message})
		if got := CausalGroupSignature(detail, groupOf("10")); got != "" {
			t.Fatalf("message=%q produced signature %q, want empty", message, got)
		}
	}
}

// Two causal groups resolving to one signature means it identifies more than one
// cause, so a verdict recorded under it could answer the wrong one. Neither keeps
// it: losing memory costs a re-investigation, conflating causes gives a wrong
// answer.
func TestAssignSignaturesClearsAmbiguousMatches(t *testing.T) {
	detail := signatureJob("job-1",
		failedBuild{"10", "TestReconcile", "timed out"},
		failedBuild{"11", "TestReconcile", "timed out"},
		failedBuild{"12", "TestDistinct", "connection refused"},
	)
	detail.PatternAnalyses = []models.PatternAnalysis{{
		JobID: "job-1", Systemic: true, CausalGroups: []models.PatternCausalGroup{
			groupOf("10"), groupOf("11"), groupOf("12"),
		},
	}}
	ApplyCausalGroupSignatures(&detail)
	groups := detail.PatternAnalyses[0].CausalGroups
	if groups[0].Signature != "" || groups[1].Signature != "" {
		t.Fatalf("two groups kept one shared signature: %q and %q", groups[0].Signature, groups[1].Signature)
	}
	if groups[2].Signature == "" {
		t.Fatal("an unambiguous group lost its signature")
	}
}

// A pre-existing signature that a newly regrouped cause now also resolves to is
// just as dangerous, so the backfill path clears the ambiguity the same way.
func TestBackfillClearsAmbiguityAgainstAnExistingSignature(t *testing.T) {
	detail := signatureJob("job-1",
		failedBuild{"10", "TestReconcile", "timed out"},
		failedBuild{"11", "TestReconcile", "timed out"},
	)
	existing := CausalGroupSignature(detail, groupOf("10"))
	if existing == "" {
		t.Fatal("no baseline signature derived")
	}
	detail.PatternAnalyses = []models.PatternAnalysis{{
		JobID: "job-1", Systemic: true, CausalGroups: []models.PatternCausalGroup{
			{Builds: []string{"999"}, RootCause: "aged out", Confidence: "high", Signature: existing},
			groupOf("11"),
		},
	}}
	BackfillCausalGroupSignatures(&detail)
	groups := detail.PatternAnalyses[0].CausalGroups
	if groups[0].Signature != "" || groups[1].Signature != "" {
		t.Fatalf("ambiguous signatures survived: %q and %q", groups[0].Signature, groups[1].Signature)
	}
}

// Numbers are often the only thing separating causes that need different answers,
// so the signature keeps them even though the flakiness normalization collapses
// them. Collapsing them here would let a conclusion about a 401 be reused for a
// 503. The cost is recall: a message whose numbers vary yields no memory, which
// only means another investigation.
func TestCausalGroupSignatureTreatsNumbersAsLoadBearing(t *testing.T) {
	unauthorized := signatureJob("job-1", failedBuild{"10", "TestReconcile", "request failed with status 401"})
	unavailable := signatureJob("job-1", failedBuild{"10", "TestReconcile", "request failed with status 503"})
	left := CausalGroupSignature(unauthorized, groupOf("10"))
	right := CausalGroupSignature(unavailable, groupOf("10"))
	if left == "" || right == "" {
		t.Fatalf("signatures=%q,%q want both derived", left, right)
	}
	if left == right {
		t.Fatal("two different status codes collapsed into one cause")
	}
	// The flakiness normalization deliberately does collapse them, which is why
	// the signature cannot reuse it.
	if aggregator.NormalizeErrorMessage("request failed with status 401") !=
		aggregator.NormalizeErrorMessage("request failed with status 503") {
		t.Fatal("flakiness normalization no longer collapses numbers; revisit the signature contract")
	}
}

// Every build-level stand-in carries the same synthesized name and message, so
// keying on it would give unrelated infrastructure failures one identity and let
// a verdict for one be reused for another.
func TestCausalGroupSignatureDeclinesBuildLevelStandIns(t *testing.T) {
	standIn := models.NewProwJobExecutionFailure(12)
	standIn.AISummary = &models.AISummary{Summary: "failure"}
	standIn.AIAnalysis = &models.AIAnalysis{RootCause: "cause", Severity: "High", Mode: "agentic", Disposition: models.AnalysisDispositionGrounded}
	detail := models.JobDetail{Name: "job-1", JobID: "job-1", Runs: []models.BuildResult{{
		BuildInfo: models.BuildInfo{BuildID: "10", Result: "FAILURE"},
		TestCases: []models.TestCase{standIn},
	}}}
	if got := CausalGroupSignature(detail, groupOf("10")); got != "" {
		t.Fatalf("signature=%q for a build-level stand-in, want empty", got)
	}
}

func TestApplyCausalGroupSignaturesKeepsPriorValueWhenBuildsAgeOut(t *testing.T) {
	detail := signatureJob("job-1", failedBuild{"10", "TestReconcile", "timed out"})
	detail.PatternAnalyses = []models.PatternAnalysis{{
		JobID: "job-1", Systemic: true, CausalGroups: []models.PatternCausalGroup{
			groupOf("10"),
			{Builds: []string{"999"}, RootCause: "aged out", Confidence: "high", Signature: "prior-signature"},
		},
	}}
	ApplyCausalGroupSignatures(&detail)
	groups := detail.PatternAnalyses[0].CausalGroups
	if groups[0].Signature == "" {
		t.Fatal("a resolvable group was not assigned a signature")
	}
	if groups[1].Signature != "prior-signature" {
		t.Fatalf("an aged-out group lost its identity: %q", groups[1].Signature)
	}
}

// A retained verdict keeps its original grouping while its builds age out, so
// recomputing could flip the dominant failure and break the continuity the
// signature exists to hold.
func TestBackfillCausalGroupSignaturesOnlyFillsMissingValues(t *testing.T) {
	detail := signatureJob("job-1",
		failedBuild{"10", "TestReconcile", "timed out"},
		failedBuild{"11", "TestOther", "connection refused"},
	)
	detail.PatternAnalyses = []models.PatternAnalysis{{
		JobID: "job-1", Systemic: true, CausalGroups: []models.PatternCausalGroup{
			{Builds: []string{"11"}, RootCause: "cause", Confidence: "high", Signature: "kept"},
			groupOf("10"),
		},
	}}
	BackfillCausalGroupSignatures(&detail)
	groups := detail.PatternAnalyses[0].CausalGroups
	if groups[0].Signature != "kept" {
		t.Fatalf("an existing signature was recomputed: %q", groups[0].Signature)
	}
	if groups[1].Signature == "" {
		t.Fatal("a missing signature was not backfilled")
	}
	ApplyCausalGroupSignatures(nil)
	BackfillCausalGroupSignatures(nil)
}

func TestMergeLastGoodAssignsAndPreservesSignatures(t *testing.T) {
	fresh := signatureJob("job-1",
		failedBuild{"12", "TestReconcile", "timed out"},
		failedBuild{"11", "TestReconcile", "timed out"},
		failedBuild{"10", "TestReconcile", "timed out"},
	)
	fresh.PatternAnalyses = []models.PatternAnalysis{{
		JobID: "job-1", Subject: "job-1", Systemic: true, Confidence: "high",
		SharedRootCause: "shared cause", SharedBuilds: []string{"10", "11"},
		CausalGroups: []models.PatternCausalGroup{groupOf("10", "11")},
		Summary:      "shared failure",
	}}
	details := []models.JobDetail{fresh}
	result := AnalyzeResult{Outcomes: map[string]JobOutcome{"job-1": {JobID: "job-1", Succeeded: true, Systemic: true}}}
	if _, err := MergeLastGood(details, map[string]models.JobDetail{}, result); err != nil {
		t.Fatal(err)
	}
	assigned := details[0].PatternAnalyses[0].CausalGroups[0].Signature
	if assigned == "" {
		t.Fatal("a current pattern was published without a signature")
	}

	// The next pass fails correlation and the builds have rolled away, so the
	// prior verdict is retained. Its identity must come along with it.
	prior := map[string]models.JobDetail{"job-1": details[0]}
	stale := signatureJob("job-1",
		failedBuild{"90", "TestSomethingElse", "unrelated"},
		failedBuild{"91", "TestSomethingElse", "unrelated"},
		failedBuild{"92", "TestSomethingElse", "unrelated"},
	)
	next := []models.JobDetail{stale}
	report, err := MergeLastGood(next, prior, AnalyzeResult{Outcomes: map[string]JobOutcome{"job-1": {JobID: "job-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Retained != 1 {
		t.Fatalf("report=%+v, want the prior verdict retained", report)
	}
	if got := next[0].PatternAnalyses[0].CausalGroups[0].Signature; got != assigned {
		t.Fatalf("retained signature=%q, want the published %q", got, assigned)
	}
}

// A flake's message routinely carries a number that changes every run, such as a
// Ginkgo timeout duration. The recurrence identity has to survive that or every
// occurrence looks like a brand new cause and no history ever accumulates.
func TestBuildRecurrenceSignatureSurvivesVaryingNumbers(t *testing.T) {
	detail := signatureJob("job-1",
		failedBuild{"10", "TestReconcile", "Timed out after 3600.001s waiting for 3 machines"},
		failedBuild{"11", "TestReconcile", "Timed out after 3612.487s waiting for 2 machines"},
	)
	first := BuildRecurrenceSignature(detail, &detail.Runs[0])
	if first == "" {
		t.Fatal("no recurrence signature derived from an analyzed failure")
	}
	if second := BuildRecurrenceSignature(detail, &detail.Runs[1]); second != first {
		t.Fatalf("signature=%q, want %q despite the varying numbers", second, first)
	}
	// The verdict-bearing identity must stay strict, or a conclusion about one
	// failure could be reused to answer a materially different one.
	if CausalGroupSignature(detail, groupOf("10")) == CausalGroupSignature(detail, groupOf("11")) {
		t.Fatal("the causal group signature collapsed numbers, so a verdict could answer the wrong failure")
	}
}

func TestBuildRecurrenceSignatureSeparatesDistinctFailures(t *testing.T) {
	detail := signatureJob("job-1",
		failedBuild{"10", "TestReconcile", "context deadline exceeded"},
		failedBuild{"11", "TestReconcile", "connection refused"},
		failedBuild{"12", "TestDelete", "context deadline exceeded"},
	)
	seen := map[string]bool{}
	for i := range detail.Runs {
		signature := BuildRecurrenceSignature(detail, &detail.Runs[i])
		if signature == "" {
			t.Fatalf("build %s produced no signature", detail.Runs[i].BuildID)
		}
		if seen[signature] {
			t.Fatalf("build %s reused signature %q of a different failure", detail.Runs[i].BuildID, signature)
		}
		seen[signature] = true
	}
}

// A signature only means anything if it rests on discriminating evidence, so a
// failure that carries none must stay unidentified rather than collapse every
// such build in the job onto one shared identity.
func TestBuildRecurrenceSignatureSkipsFailuresWithoutEvidence(t *testing.T) {
	standIn := models.NewProwJobExecutionFailure(12)
	standIn.AISummary = &models.AISummary{Summary: "failure"}
	standIn.AIAnalysis = &models.AIAnalysis{RootCause: "cause", Severity: "High", Mode: "agentic", Disposition: models.AnalysisDispositionGrounded}

	for name, run := range map[string]models.BuildResult{
		"build level stand in": {
			BuildInfo: models.BuildInfo{BuildID: "10", Result: "FAILURE"},
			TestCases: []models.TestCase{standIn},
		},
		"unanalyzed failure": {
			BuildInfo: models.BuildInfo{BuildID: "11", Result: "FAILURE"},
			TestCases: []models.TestCase{{Name: "TestReconcile", Status: "failed", FailureMessage: "boom"}},
		},
		"no failure message": {
			BuildInfo: models.BuildInfo{BuildID: "12", Result: "FAILURE"},
			TestCases: []models.TestCase{{
				Name: "TestReconcile", Status: "failed",
				AIAnalysis: &models.AIAnalysis{RootCause: "cause", Severity: "High", Mode: "agentic", Disposition: models.AnalysisDispositionGrounded},
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			detail := models.JobDetail{Name: "job-1", JobID: "job-1", Runs: []models.BuildResult{run}}
			if got := BuildRecurrenceSignature(detail, &detail.Runs[0]); got != "" {
				t.Fatalf("signature=%q, want empty without discriminating evidence", got)
			}
		})
	}
}

// A message with no digits normalizes identically under both rules, so only a
// domain separates them. Without it the two identities share one ledger entry and
// a causal group's builds inflate the count published for recurrence.
func TestBuildRecurrenceSignatureNeverCollidesWithTheVerdictIdentity(t *testing.T) {
	for name, message := range map[string]string{
		"no digits":   "context deadline exceeded",
		"with digits": "connection refused on port 6443",
	} {
		t.Run(name, func(t *testing.T) {
			detail := signatureJob("job-1", failedBuild{"10", "TestReconcile", message})
			group := CausalGroupSignature(detail, groupOf("10"))
			recurrence := BuildRecurrenceSignature(detail, &detail.Runs[0])
			if group == "" || recurrence == "" {
				t.Fatalf("group=%q recurrence=%q, want both derived", group, recurrence)
			}
			if group == recurrence {
				t.Fatalf("both identities are %q, so one ledger entry serves two purposes", group)
			}
		})
	}
}
