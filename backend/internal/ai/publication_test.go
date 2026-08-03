package ai

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func TestAnalysisCitationValidation(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{
		"build-log.txt": {
			Segments: []string{"error: error execution phase etcd-join"},
			Lines:    map[int]string{2494: "error:  error execution phase etcd-join"},
		},
	}
	valid := analysisResponse{
		Summary: "etcd join failed", RootCause: "The etcd join failure is shown at line 2494.",
		SuggestedFix: "Correct the etcd join configuration and rerun the job.", Severity: "High",
		EvidenceCitations: []models.EvidenceCitation{{
			Path: "build-log.txt", LineStart: 2494, LineEnd: 2494,
			Quote: "error: error execution phase etcd-join",
		}},
	}
	out := critiqueDraftWithContent(valid, nil, nil, nil, nil, nil, 0, analysisCitationContext{Evidence: evidence})
	if !out.Passed || len(out.CitationIssues) != 0 {
		t.Fatalf("valid citation outcome = %+v", out)
	}

	wrongLine := valid
	wrongLine.RootCause = "The etcd join failure is shown at line 73."
	out = critiqueDraftWithContent(wrongLine, nil, nil, nil, nil, nil, 0, analysisCitationContext{Evidence: evidence})
	if out.Passed || !strings.Contains(strings.Join(out.CitationIssues, " "), "line claim 73-73") {
		t.Fatalf("wrong-line outcome = %+v", out)
	}
	sanitized := sanitizePublishedCitations(wrongLine, analysisCitationContext{Evidence: evidence})
	if strings.Contains(sanitized.RootCause, "line 73") || len(sanitized.EvidenceCitations) != 1 {
		t.Fatalf("sanitized wrong-line analysis = %+v", sanitized)
	}

	wrongPath := valid
	wrongPath.RootCause = "The etcd join failure is shown in other.log at line 2494."
	out = critiqueDraftWithContent(wrongPath, nil, nil, nil, nil, nil, 0, analysisCitationContext{Evidence: evidence})
	if out.Passed || !strings.Contains(strings.Join(out.CitationIssues, " "), "other.log:2494-2494") {
		t.Fatalf("wrong-path outcome = %+v", out)
	}
	sanitized = sanitizePublishedCitations(wrongPath, analysisCitationContext{Evidence: evidence})
	if strings.Contains(sanitized.RootCause, "line 2494") {
		t.Fatalf("wrong-path line claim was published: %+v", sanitized)
	}

	wrongSuffixPath := valid
	wrongSuffixPath.RootCause = "The etcd join failure is shown at line 2494 in other.log."
	out = critiqueDraftWithContent(wrongSuffixPath, nil, nil, nil, nil, nil, 0, analysisCitationContext{Evidence: evidence})
	if out.Passed || !strings.Contains(strings.Join(out.CitationIssues, " "), "other.log:2494-2494") {
		t.Fatalf("suffix-path outcome = %+v", out)
	}
}

func TestWideCitationDoesNotValidateDifferentProseLine(t *testing.T) {
	lines := make(map[int]string, 200)
	for line := 1; line <= 200; line++ {
		lines[line] = "context"
	}
	lines[150] = "error execution phase etcd-join"
	parsed := analysisResponse{
		RootCause: "The failure is shown at line 73.",
		EvidenceCitations: []models.EvidenceCitation{{
			Path: "build-log.txt", LineStart: 1, LineEnd: 200, Quote: "error execution phase etcd-join",
		}},
	}
	context := analysisCitationContext{Evidence: map[string]*analysisChatEvidence{"build-log.txt": {Lines: lines}}}
	out := critiqueDraftWithContent(parsed, nil, nil, nil, nil, nil, 0, context)
	if out.Passed || len(out.CitationIssues) == 0 {
		t.Fatalf("wide citation validated the wrong prose line: %+v", out)
	}
	if sanitized := sanitizePublishedCitations(parsed, context); strings.Contains(sanitized.RootCause, "line 73") {
		t.Fatalf("wrong line survived sanitization: %+v", sanitized)
	}
}

func TestEvidenceCitationRangeOverflowIsRejected(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	citation := models.EvidenceCitation{Path: "build-log.txt", LineStart: 1, LineEnd: maxInt, Quote: "error text"}
	evidence := map[string]*analysisChatEvidence{"build-log.txt": {Lines: map[int]string{1: "error text"}}}
	if issue := evidenceCitationIssue(citation, evidence); !strings.Contains(issue, "invalid line range") {
		t.Fatalf("overflowing range issue = %q", issue)
	}
}

func TestInvalidNumericProseLineClaimsAreRemoved(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, text := range []string{
		"The failure is shown at line 0.",
		fmt.Sprintf("The failure is shown at line %d0.", maxInt),
		"The failure is shown at lines 73-42.",
	} {
		parsed := analysisResponse{RootCause: text}
		out := critiqueDraftWithContent(parsed, nil, nil, nil, nil, nil, 0, analysisCitationContext{Evidence: map[string]*analysisChatEvidence{}})
		if out.Passed || len(out.CitationIssues) == 0 {
			t.Fatalf("invalid numeric claim %q passed: %+v", text, out)
		}
		sanitized := sanitizePublishedCitations(parsed, analysisCitationContext{Evidence: map[string]*analysisChatEvidence{}})
		if sanitized.RootCause == text {
			t.Fatalf("invalid numeric claim survived: %q", sanitized.RootCause)
		}
	}
}

func TestKernelTimestampIsNotLineClaim(t *testing.T) {
	parsed := analysisResponse{
		Summary: "kernel delay", RootCause: "The kernel timestamp 73.123 seconds precedes the timeout.",
		SuggestedFix: "Correct the timeout configuration and rerun the job.", Severity: "Medium",
	}
	out := critiqueDraftWithContent(parsed, nil, nil, nil, nil, nil, 0, analysisCitationContext{Evidence: map[string]*analysisChatEvidence{}})
	if !out.Passed || len(out.CitationIssues) != 0 {
		t.Fatalf("timestamp outcome = %+v", out)
	}
}

func TestPreparePublishedAnalysisFiltersPathsAndCLIFlags(t *testing.T) {
	state := &agentState{
		readArtifactsFull: map[string]bool{"config/observed.yaml": true},
		readArtifactsBase: map[string]bool{"observed.yaml": true},
		readSourceFull: map[string]bool{
			"scripts/ci-e2e.sh": true,
			"makefile":          true,
		},
		sourceContentByPath: map[string][]string{
			"makefile": {"ci-e2e: ./scripts/ci-e2e.sh"},
		},
	}
	parsed := analysisResponse{
		RootCause: "AKS lifecycle configuration using --enable-long-term-support caused the build failure.", Severity: "High",
		SuggestedFix:  "Update config/observed.yaml and scripts/aks-create.sh, then run az aks update --enable-long-term-support --lts-version 1.2 and rerun the job.",
		RelevantFiles: []string{"build-log.txt", "scripts/aks-create.sh", "scripts/ci-e2e.sh", "Makefile"},
	}
	got := state.preparePublishedAnalysis(parsed)
	if !slices.Equal(got.RelevantFiles, []string{"scripts/ci-e2e.sh", "Makefile"}) {
		t.Fatalf("relevant files = %v", got.RelevantFiles)
	}
	if !slices.Contains(got.SearchSuggestions, "scripts/aks-create.sh") {
		t.Fatalf("search suggestions = %v", got.SearchSuggestions)
	}
	if slices.Contains(got.SearchSuggestions, "build-log.txt") {
		t.Fatalf("artifact path became a source search suggestion: %v", got.SearchSuggestions)
	}
	if strings.Contains(got.SuggestedFix, "config/observed.yaml") || strings.Contains(got.SuggestedFix, "scripts/aks-create.sh") || strings.Contains(got.SuggestedFix, "--enable-") || strings.Contains(got.SuggestedFix, "--lts-") {
		t.Fatalf("unsupported remediation leaked: %q", got.SuggestedFix)
	}
	if strings.Contains(got.RootCause, "--enable-long-term-support") || !strings.Contains(got.RootCause, "AKS lifecycle configuration") {
		t.Fatalf("root cause was not safely preserved: %q", got.RootCause)
	}
}

func TestQualifiedArtifactPathRequiresExactRead(t *testing.T) {
	state := &agentState{
		readArtifactsFull: map[string]bool{"artifacts/observed.yaml": true},
		readArtifactsBase: map[string]bool{"observed.yaml": true},
	}
	parsed := analysisResponse{RootCause: "config/observed.yaml shows the failure; observed.yaml confirms it."}
	got := state.preparePublishedAnalysis(parsed).RootCause
	if strings.Contains(got, "config/observed.yaml") || !strings.Contains(got, "observed.yaml confirms") {
		t.Fatalf("qualified artifact path used a basename-only read: %q", got)
	}
}

func TestPreparePublishedAnalysisKeepsGroundedFlag(t *testing.T) {
	state := &agentState{sourceContentByPath: map[string][]string{"Makefile": {"tool --supported"}}}
	parsed := analysisResponse{SuggestedFix: "Run tool --supported and rerun the job."}
	if got := state.preparePublishedAnalysis(parsed).SuggestedFix; !strings.Contains(got, "--supported") {
		t.Fatalf("grounded flag removed: %q", got)
	}
}

func TestPreparePublishedAnalysisFallsBackForAnyUnsupportedRemediationFlag(t *testing.T) {
	for _, fix := range []string{
		"Run helm uninstall --dry-run release.",
		"Run kubectl apply -f guessed.yaml.",
		"Run helm uninstall -n prod release.",
	} {
		got := (&agentState{}).preparePublishedAnalysis(analysisResponse{SuggestedFix: fix}).SuggestedFix
		if strings.Contains(got, "helm uninstall") || strings.Contains(got, "kubectl apply") || !strings.Contains(got, "verified project automation") {
			t.Fatalf("unsafe command rewrite was published: input=%q output=%q", fix, got)
		}
	}
}

func TestPreparePublishedAnalysisKeepsGroundedShortFlags(t *testing.T) {
	state := &agentState{sourceContentByPath: map[string][]string{"Makefile": {"tool -abc", "helm uninstall -nprod release"}}}
	for _, fix := range []string{"Run tool -abc.", "Run helm uninstall -nprod release."} {
		if got := state.preparePublishedAnalysis(analysisResponse{SuggestedFix: fix}).SuggestedFix; got != fix {
			t.Fatalf("grounded short flag changed: input=%q output=%q", fix, got)
		}
	}
}

func TestPreparePublishedAnalysisFallsBackForUngroundedPathInCommand(t *testing.T) {
	state := &agentState{sourceContentByPath: map[string][]string{"Makefile": {"kubectl apply -f"}}}
	parsed := analysisResponse{
		SuggestedFix: "Run kubectl apply -f guessed.yaml.",
	}
	got := state.preparePublishedAnalysis(parsed).SuggestedFix
	if strings.Contains(got, "kubectl apply") || strings.Contains(got, "guessed.yaml") || !strings.Contains(got, "verified project automation") {
		t.Fatalf("ungrounded command path was rewritten inline: %q", got)
	}
}

func TestPreparePublishedAnalysisRequiresExactFlagGrounding(t *testing.T) {
	state := &agentState{sourceContentByPath: map[string][]string{"Makefile": {"tool --supported-extra"}}}
	parsed := analysisResponse{SuggestedFix: "Run tool --supported and rerun the job."}
	if got := state.preparePublishedAnalysis(parsed).SuggestedFix; strings.Contains(got, "--supported") {
		t.Fatalf("substring-grounded flag survived: %q", got)
	}
}

func TestRecordSourceContentFromVisibleGrepPayload(t *testing.T) {
	state := &agentState{}
	state.recordSourceContent(modelToolCall{Function: modelFunction{Name: "grep_repo"}}, map[string]interface{}{
		"matches": []interface{}{map[string]interface{}{
			"path": "Makefile", "context": []interface{}{"> 12: tool --supported"},
		}},
	})
	if got := state.preparePublishedAnalysis(analysisResponse{SuggestedFix: "Run tool --supported and rerun the job."}).SuggestedFix; !strings.Contains(got, "--supported") {
		t.Fatalf("visible grep grounding was not recorded: %q", got)
	}
}

func TestResponseFormatIncludesEvidenceContract(t *testing.T) {
	for _, required := range []string{"evidence_citations", "line_start", "search_suggestions", "verified source files", "exact CLI flags"} {
		if !strings.Contains(ResponseFormatFooter, required) {
			t.Fatalf("ResponseFormatFooter missing %q", required)
		}
	}
}

func TestInvalidEvidenceCitationIsDropped(t *testing.T) {
	parsed := analysisResponse{
		RootCause:         "The failure is shown at line 10.",
		EvidenceCitations: []models.EvidenceCitation{{Path: "unread.log", LineStart: 10, LineEnd: 10, Quote: "error text"}},
	}
	context := analysisCitationContext{Evidence: map[string]*analysisChatEvidence{}}
	out := critiqueDraftWithContent(parsed, nil, nil, nil, nil, nil, 0, context)
	if out.Passed || len(out.CitationIssues) == 0 {
		t.Fatalf("invalid citation outcome = %+v", out)
	}
	sanitized := sanitizePublishedCitations(parsed, context)
	if len(sanitized.EvidenceCitations) != 0 || strings.Contains(sanitized.RootCause, "line 10") {
		t.Fatalf("invalid citation was published: %+v", sanitized)
	}
}

func TestSuggestedFixLineClaimIsSanitized(t *testing.T) {
	parsed := analysisResponse{SuggestedFix: "Update scripts/ci-e2e.sh at line 999 and rerun the job."}
	sanitized := sanitizePublishedCitations(parsed, analysisCitationContext{Evidence: map[string]*analysisChatEvidence{}})
	if strings.Contains(sanitized.SuggestedFix, "line 999") {
		t.Fatalf("unsupported suggested-fix line survived: %q", sanitized.SuggestedFix)
	}
}

func TestPathQualifiedAndBareLineClaimsRequireCitations(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{
		"build-log.txt": {Lines: map[int]string{2494: "error execution phase etcd-join"}},
	}
	citation := models.EvidenceCitation{Path: "build-log.txt", LineStart: 2494, LineEnd: 2494, Quote: "error execution phase etcd-join"}
	for _, text := range []string{
		"The failure is at build-log.txt:73.",
		"The failure is at build-log.txt#L73.",
		"The failure is at L73.",
		"The failure is at line number 73.",
	} {
		parsed := analysisResponse{RootCause: text, EvidenceCitations: []models.EvidenceCitation{citation}}
		out := critiqueDraftWithContent(parsed, nil, nil, nil, nil, nil, 0, analysisCitationContext{Evidence: evidence})
		if out.Passed || len(out.CitationIssues) == 0 {
			t.Fatalf("unsupported claim %q passed: %+v", text, out)
		}
		sanitized := sanitizePublishedCitations(parsed, analysisCitationContext{Evidence: evidence})
		if strings.Contains(sanitized.RootCause, "73") {
			t.Fatalf("unsupported claim %q survived as %q", text, sanitized.RootCause)
		}
	}
	valid := analysisResponse{RootCause: "The failure is at build-log.txt:2494.", EvidenceCitations: []models.EvidenceCitation{citation}}
	if out := critiqueDraftWithContent(valid, nil, nil, nil, nil, nil, 0, analysisCitationContext{Evidence: evidence}); !out.Passed {
		t.Fatalf("valid path-qualified claim failed: %+v", out)
	}
}

func TestSourceLineClaimsCannotUseArtifactCitations(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{
		"build-log.txt": {Lines: map[int]string{999: "source line claim"}},
	}
	citation := models.EvidenceCitation{Path: "build-log.txt", LineStart: 999, LineEnd: 999, Quote: "source line claim"}
	for _, text := range []string{
		"The failure is in scripts/ci-e2e.sh:999.",
		"The failure is in scripts/ci-e2e.sh at line 999.",
	} {
		parsed := analysisResponse{RootCause: text, EvidenceCitations: []models.EvidenceCitation{citation}}
		out := critiqueDraftWithContent(parsed, nil, nil, nil, nil, nil, 0, analysisCitationContext{Evidence: evidence})
		if out.Passed || len(out.CitationIssues) == 0 {
			t.Fatalf("source claim %q used an artifact citation: %+v", text, out)
		}
		sanitized := sanitizePublishedCitations(parsed, analysisCitationContext{Evidence: evidence})
		if strings.Contains(sanitized.RootCause, "999") {
			t.Fatalf("source line claim %q survived as %q", text, sanitized.RootCause)
		}
	}
}

func TestEvidenceOverflowOnlyBlocksLineAwareDrafts(t *testing.T) {
	context := analysisCitationContext{Full: true}
	plain := analysisResponse{RootCause: "The controller failed after the API became unavailable."}
	if out := critiqueDraftWithContent(plain, nil, nil, nil, nil, nil, 0, context); !out.Passed {
		t.Fatalf("plain draft was blocked by unused evidence overflow: %+v", out)
	}
	lineAware := analysisResponse{RootCause: "The controller failed at line 42."}
	if out := critiqueDraftWithContent(lineAware, nil, nil, nil, nil, nil, 0, context); out.Passed || len(out.CitationIssues) == 0 {
		t.Fatalf("line-aware draft ignored evidence overflow: %+v", out)
	}
}

func TestCappedToolPayloadCannotGroundHiddenEvidence(t *testing.T) {
	state := &agentState{opts: AgenticOptions{ModelByteBudget: 100_000, GCSByteBudget: 100_000}, startTime: time.Now()}
	payload := map[string]interface{}{
		"matches": []interface{}{
			map[string]interface{}{"context": []interface{}{"> 1: " + strings.Repeat("x", agenticToolBudget)}},
			map[string]interface{}{"context": []interface{}{"> 2494: hidden evidence"}},
		},
	}
	visible := modelVisibleToolPayload(toolEnvelopeJSON(state, payload))
	evidence := map[string]*analysisChatEvidence{}
	call := modelToolCall{Function: modelFunction{Name: "grep_artifact", Arguments: `{"path":"build-log.txt"}`}}
	recordAnalysisChatEvidence(evidence, call, visible)
	if issue := evidenceCitationIssue(models.EvidenceCitation{Path: "build-log.txt", LineStart: 2494, LineEnd: 2494, Quote: "hidden evidence"}, evidence); issue == "" {
		t.Fatal("hidden capped evidence was accepted")
	}
}

func TestPublishedEvidenceCitationsAreBounded(t *testing.T) {
	evidence := map[string]*analysisChatEvidence{"build-log.txt": {Lines: map[int]string{1: "error text"}}}
	parsed := analysisResponse{}
	for range 21 {
		parsed.EvidenceCitations = append(parsed.EvidenceCitations, models.EvidenceCitation{
			Path: "build-log.txt", LineStart: 1, LineEnd: 1, Quote: "error text",
		})
	}
	sanitized := sanitizePublishedCitations(parsed, analysisCitationContext{Evidence: evidence})
	if len(sanitized.EvidenceCitations) != 20 {
		t.Fatalf("published citations = %d, want 20", len(sanitized.EvidenceCitations))
	}
}
