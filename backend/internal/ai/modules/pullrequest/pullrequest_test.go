package pullrequest

import (
	"context"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai/modules/universal"
	"github.com/willie-yao/aster/backend/internal/models"
)

func testSubject() Subject {
	return Subject{
		Number: 6209, HeadSHA: "553f8cf", BaseRef: "main",
		Files: []ChangedFile{
			{Path: "azure/scope/cluster.go", Status: "modified", Patch: "@@ -1,2 +1,3 @@\n+added\n"},
			{Path: "azure/mock_azure/azure_mock.go", Status: "modified", Generated: true},
		},
	}
}

func promptFor(t *testing.T, subject Subject) string {
	t.Helper()
	run := &models.BuildResult{BuildInfo: models.BuildInfo{BuildID: "100", WebURL: "https://example.test/100"}}
	tc := &models.TestCase{Name: "[It] creates a cluster", Status: "failed", FailureMessage: "boom"}
	return New(subject).AnalysisPrompt(context.Background(), nil, run, tc, 1)
}

func TestNameIsolatesTheCacheFromTheDashboardAnalysis(t *testing.T) {
	if got := New(testSubject()).Name(); got != ModuleName {
		t.Fatalf("Name = %q, want %q", got, ModuleName)
	}
	// The agentic cache key is built from the module name, so colliding with
	// the universal module would let a pull request analysis be served as the
	// dashboard's analysis of the same failure.
	if ModuleName == universal.New().Name() {
		t.Fatal("the pull request module must not share the universal module's name")
	}
}

// The universal seed prompt carries the investigation instructions every other
// analysis is gated on, so it must survive intact.
func TestPromptKeepsTheUniversalInvestigationSeed(t *testing.T) {
	subject := testSubject()
	run := &models.BuildResult{BuildInfo: models.BuildInfo{BuildID: "100"}}
	tc := &models.TestCase{Name: "[It] creates a cluster", Status: "failed"}
	base := universal.New().AnalysisPrompt(context.Background(), nil, run, tc, 1)

	got := New(subject).AnalysisPrompt(context.Background(), nil, run, tc, 1)
	if !strings.HasPrefix(got, base) {
		t.Fatal("the pull request prompt must extend the universal seed, not replace it")
	}
}

func TestPromptNamesThePullRequestAndItsFiles(t *testing.T) {
	got := promptFor(t, testSubject())

	for _, want := range []string{"#6209", "main", "553f8cf", "azure/scope/cluster.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if !strings.Contains(got, "(generated)") {
		t.Error("generated files should be labelled so the model discounts them")
	}
}

// The prompt must actively discourage inventing a link to the diff, which is
// the central risk of showing a model the change alongside a failure.
func TestPromptForbidsClaimingCausation(t *testing.T) {
	got := promptFor(t, testSubject())

	for _, want := range []string{
		"Do not claim the pull request caused the failure",
		"context for your investigation, not evidence",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing the guard %q", want)
		}
	}
}

func TestTruncatedFileListWarnsAgainstConcludingFromAbsence(t *testing.T) {
	subject := testSubject()
	subject.FilesTruncated = true

	got := promptFor(t, subject)
	if !strings.Contains(got, "Do not conclude that a file is unchanged because it is absent") {
		t.Fatal("a truncated list must warn against reasoning from absence")
	}
	if strings.Contains(promptFor(t, testSubject()), "Do not conclude that a file is unchanged") {
		t.Error("a complete list must not carry the truncation warning")
	}
}

func TestMissingFilesDegradeToAnOrdinaryInvestigation(t *testing.T) {
	got := promptFor(t, Subject{Number: 6209})

	if !strings.Contains(got, "changed-file list is unavailable") {
		t.Fatal("an empty file set should say so")
	}
	if strings.Contains(got, "Change hunks:") {
		t.Error("no files means no patch section")
	}
}

func TestHandWrittenFilesAreListedBeforeGeneratedOnes(t *testing.T) {
	subject := Subject{Number: 1, Files: []ChangedFile{
		{Path: "aaa_generated.go", Generated: true},
		{Path: "zzz_source.go"},
	}}
	got := promptFor(t, subject)

	if strings.Index(got, "zzz_source.go") > strings.Index(got, "aaa_generated.go") {
		t.Fatal("hand-written files should lead the listing")
	}
}

func TestListedFilesAreBounded(t *testing.T) {
	subject := Subject{Number: 1}
	for i := 0; i < maxListedFiles*2; i++ {
		subject.Files = append(subject.Files, ChangedFile{Path: strings.Repeat("a", 3) + string(rune('a'+i%26)) + ".go"})
	}
	got := promptFor(t, subject)

	// Count only inside the file listing; the universal seed has bullets too.
	start := strings.Index(got, "Files this pull request changes:")
	end := strings.Index(got, "more files not listed")
	if start < 0 || end < 0 {
		t.Fatalf("prompt missing the bounded listing:\n%s", got)
	}
	if listed := strings.Count(got[start:end], ".go\n"); listed != maxListedFiles {
		t.Fatalf("listed files = %d, want exactly %d", listed, maxListedFiles)
	}
}

func TestPatchTextIsBounded(t *testing.T) {
	subject := Subject{Number: 1, Files: []ChangedFile{
		{Path: "huge.go", Patch: strings.Repeat("x", maxPatchBytes*2)},
		{Path: "small.go", Patch: "@@ -1 +1 @@\n+ok\n"},
	}}
	got := promptFor(t, subject)

	if strings.Contains(got, strings.Repeat("x", 100)) {
		t.Error("an oversized patch must not be rendered")
	}
	if !strings.Contains(got, "+ok") {
		t.Error("a small patch should still be rendered")
	}
}

func TestGeneratedFilePatchesAreNotRenderedWhenAbsent(t *testing.T) {
	// The caller drops patches for generated paths, so the prompt simply has
	// nothing to render for them.
	got := promptFor(t, testSubject())

	if strings.Contains(got, "azure_mock.go\n@@") {
		t.Error("a generated file should not contribute patch text")
	}
	if !strings.Contains(got, "Change hunks:") {
		t.Error("the hand-written patch should be rendered")
	}
}
