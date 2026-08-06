package agentanalysis

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type testSourceReader struct {
	files map[string]string
	calls map[string]int
	err   error
}

func (r *testSourceReader) ReadFile(_ context.Context, _ sourceinvestigation.Repository, path string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[path]++
	content, ok := r.files[path]
	if !ok {
		return "", errors.New("missing")
	}
	return content, nil
}

func validAnalysisJSON(bundle EvidenceBundle) string {
	return `{"version":1,"contract_version":"agent-analysis-v1","summary":"request failed","is_transient":false,"root_cause":"the artifact records a failure","severity":"High","suggested_fix":"Correct the retry behavior.","relevant_files":["build-log.txt","pkg/retry.go"],"evidence_citations":[{"excerpt_id":"` + bundle.Excerpts[0].ID + `","line_start":2,"line_end":2,"quote":"failure text"}],"source_citations":[{"path":"pkg/retry.go","line_start":2,"line_end":2,"quote":"return err"}],"unresolved_details":[]}`
}

func TestParseAndValidateAnalysis(t *testing.T) {
	bundle := testBundle(t)
	reader := &testSourceReader{files: map[string]string{"pkg/retry.go": "func retry() {\nreturn err\n}\n"}}
	got, err := parseAndValidateAnalysis(t.Context(), validAnalysisJSON(bundle), bundle, reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.EvidenceCitations) != 1 || len(got.SourceCitations) != 1 || !got.SourceCitations[0].Verified {
		t.Fatalf("analysis = %+v", got)
	}
	if reader.calls["pkg/retry.go"] != 1 {
		t.Fatalf("source reads = %v", reader.calls)
	}
}

func TestParseAndValidateAnalysisRepairsEvidenceCitationRanges(t *testing.T) {
	bundle, err := NewEvidenceBundle(
		testRequest(),
		sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)},
		ArtifactScan{PathCount: 1}, nil,
		[]EvidenceExcerpt{{Path: "build-log.txt", Kind: "tail", Content: "prefix\n2026-08-06 Node is NotReady worker\n2026-08-06 Node is NotReady worker2\n"}}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	quote := "Node is NotReady worker\nNode is NotReady worker2"
	raw := fmt.Sprintf(`{"version":1,"contract_version":"agent-analysis-v1","summary":"request failed","is_transient":false,"root_cause":"the artifact records a failure","severity":"High","suggested_fix":"Correct the retry behavior.","relevant_files":["build-log.txt","pkg/retry.go"],"evidence_citations":[{"excerpt_id":%q,"line_start":1,"line_end":1,"quote":%q}],"source_citations":[{"path":"pkg/retry.go","line_start":2,"line_end":2,"quote":"return err"}],"unresolved_details":[]}`, bundle.Excerpts[0].ID, quote)
	got, err := parseAndValidateAnalysis(t.Context(), raw, bundle, &testSourceReader{files: map[string]string{"pkg/retry.go": "func retry() {\nreturn err\n}\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if citation := got.EvidenceCitations[0]; citation.LineStart != 2 || citation.LineEnd != 3 || citation.Quote != quote {
		t.Fatalf("citation = %+v", citation)
	}
}

func TestParseAndValidateAnalysisRejectsNonconsecutiveEvidenceQuote(t *testing.T) {
	bundle, err := NewEvidenceBundle(
		testRequest(),
		sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)},
		ArtifactScan{PathCount: 1}, nil,
		[]EvidenceExcerpt{{Path: "build-log.txt", Kind: "tail", Content: "first signal\nunrelated\nsecond signal\n"}}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"version":1,"contract_version":"agent-analysis-v1","summary":"request failed","is_transient":false,"root_cause":"the artifact records a failure","severity":"High","suggested_fix":"Correct the retry behavior.","relevant_files":["build-log.txt","pkg/retry.go"],"evidence_citations":[{"excerpt_id":%q,"line_start":1,"line_end":3,"quote":"first signal\nsecond signal"}],"source_citations":[{"path":"pkg/retry.go","line_start":2,"line_end":2,"quote":"return err"}],"unresolved_details":[]}`, bundle.Excerpts[0].ID)
	if _, err := parseAndValidateAnalysis(t.Context(), raw, bundle, &testSourceReader{files: map[string]string{"pkg/retry.go": "func retry() {\nreturn err\n}\n"}}); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("error = %v", err)
	}
}

func TestParseAndValidateAnalysisRejectsBlankQuoteWildcard(t *testing.T) {
	bundle, err := NewEvidenceBundle(
		testRequest(),
		sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)},
		ArtifactScan{PathCount: 1}, nil,
		[]EvidenceExcerpt{{Path: "build-log.txt", Kind: "tail", Content: "first signal\nunrelated content\nsecond signal\n"}}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"version":1,"contract_version":"agent-analysis-v1","summary":"request failed","is_transient":false,"root_cause":"the artifact records a failure","severity":"High","suggested_fix":"Correct the retry behavior.","relevant_files":["build-log.txt","pkg/retry.go"],"evidence_citations":[{"excerpt_id":%q,"line_start":1,"line_end":3,"quote":"first signal\n\nsecond signal"}],"source_citations":[{"path":"pkg/retry.go","line_start":2,"line_end":2,"quote":"return err"}],"unresolved_details":[]}`, bundle.Excerpts[0].ID)
	if _, err := parseAndValidateAnalysis(t.Context(), raw, bundle, &testSourceReader{files: map[string]string{"pkg/retry.go": "func retry() {\nreturn err\n}\n"}}); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("error = %v", err)
	}
}

func TestParseAndValidateAnalysisUsesHintForRepeatedQuote(t *testing.T) {
	bundle, err := NewEvidenceBundle(
		testRequest(),
		sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)},
		ArtifactScan{PathCount: 1}, nil,
		[]EvidenceExcerpt{{Path: "build-log.txt", Kind: "tail", Content: "same signal\nother\nsame signal\n"}}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"version":1,"contract_version":"agent-analysis-v1","summary":"request failed","is_transient":false,"root_cause":"the artifact records a failure","severity":"High","suggested_fix":"Correct the retry behavior.","relevant_files":["build-log.txt","pkg/retry.go"],"evidence_citations":[{"excerpt_id":%q,"line_start":3,"line_end":3,"quote":"same signal"}],"source_citations":[{"path":"pkg/retry.go","line_start":2,"line_end":2,"quote":"return err"}],"unresolved_details":[]}`, bundle.Excerpts[0].ID)
	got, err := parseAndValidateAnalysis(t.Context(), raw, bundle, &testSourceReader{files: map[string]string{"pkg/retry.go": "func retry() {\nreturn err\n}\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if citation := got.EvidenceCitations[0]; citation.LineStart != 3 || citation.LineEnd != 3 {
		t.Fatalf("citation = %+v", citation)
	}
}

func TestParseAndValidateAnalysisRejectsAmbiguousQuoteWithoutHint(t *testing.T) {
	bundle, err := NewEvidenceBundle(
		testRequest(),
		sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)},
		ArtifactScan{PathCount: 1}, nil,
		[]EvidenceExcerpt{{Path: "build-log.txt", Kind: "tail", Content: "same signal\nother\nsame signal\n"}}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"version":1,"contract_version":"agent-analysis-v1","summary":"request failed","is_transient":false,"root_cause":"the artifact records a failure","severity":"High","suggested_fix":"Correct the retry behavior.","relevant_files":["build-log.txt","pkg/retry.go"],"evidence_citations":[{"excerpt_id":%q,"line_start":1,"line_end":3,"quote":"same signal"}],"source_citations":[{"path":"pkg/retry.go","line_start":2,"line_end":2,"quote":"return err"}],"unresolved_details":[]}`, bundle.Excerpts[0].ID)
	if _, err := parseAndValidateAnalysis(t.Context(), raw, bundle, &testSourceReader{files: map[string]string{"pkg/retry.go": "func retry() {\nreturn err\n}\n"}}); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("error = %v", err)
	}
}

func TestParseAndValidateAnalysisRejectsMalformedOutput(t *testing.T) {
	bundle := testBundle(t)
	reader := &testSourceReader{files: map[string]string{"pkg/retry.go": "return err\n"}}
	tests := map[string]string{
		"unknown field":        strings.Replace(validAnalysisJSON(bundle), `"summary":"request failed"`, `"summary":"request failed","extra":true`, 1),
		"duplicate field":      strings.Replace(validAnalysisJSON(bundle), `"summary":"request failed"`, `"summary":"one","summary":"two"`, 1),
		"trailing":             validAnalysisJSON(bundle) + `{}`,
		"wrong contract":       strings.Replace(validAnalysisJSON(bundle), ContractVersion, "other", 1),
		"unknown excerpt":      strings.Replace(validAnalysisJSON(bundle), bundle.Excerpts[0].ID, "missing", 1),
		"no artifact citation": strings.Replace(validAnalysisJSON(bundle), `[{"excerpt_id":"`+bundle.Excerpts[0].ID+`","line_start":2,"line_end":2,"quote":"failure text"}]`, `[]`, 1),
		"transient mismatch":   strings.Replace(validAnalysisJSON(bundle), `"is_transient":false`, `"is_transient":true`, 1),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseAndValidateAnalysis(t.Context(), raw, bundle, reader); !errors.Is(err, ErrInvalidResult) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestParseAndValidateAnalysisDropsUngroundedRelevantFile(t *testing.T) {
	bundle := testBundle(t)
	raw := strings.Replace(validAnalysisJSON(bundle), `"build-log.txt","pkg/retry.go"`, `"build-log.txt","pkg/other.go"`, 1)
	reader := &testSourceReader{files: map[string]string{"pkg/retry.go": "func retry() {\nreturn err\n}\n"}}
	got, err := parseAndValidateAnalysis(t.Context(), raw, bundle, reader)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.RelevantFiles, []string{"build-log.txt"}) {
		t.Fatalf("relevant files = %v", got.RelevantFiles)
	}
}

func TestParseAndValidateAnalysisRejectsUnsafeRelevantFile(t *testing.T) {
	bundle := testBundle(t)
	raw := strings.Replace(validAnalysisJSON(bundle), `"build-log.txt","pkg/retry.go"`, `"../unsafe","pkg/retry.go"`, 1)
	reader := &testSourceReader{files: map[string]string{"pkg/retry.go": "func retry() {\nreturn err\n}\n"}}
	if _, err := parseAndValidateAnalysis(t.Context(), raw, bundle, reader); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateNewFileDiff(t *testing.T) {
	valid := validOutputDiff("{}")
	if err := validateNewFileDiff(valid); err != nil {
		t.Fatal(err)
	}
	for name, diff := range map[string]string{
		"modified": "diff --git a/" + OutputPath + " b/" + OutputPath + "\n--- a/" + OutputPath + "\n+++ b/" + OutputPath + "\n",
		"deleted":  valid + "\ndeleted file mode 100644\n",
		"extra":    valid + "\ndiff --git a/other b/other\n",
		"empty":    "",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateNewFileDiff(diff); !errors.Is(err, ErrInvalidResult) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func validOutputDiff(body string) string {
	return "diff --git a/" + OutputPath + " b/" + OutputPath + "\n" +
		"new file mode 100644\nindex 0000000..1111111\n--- /dev/null\n+++ b/" + OutputPath + "\n@@ -0,0 +1 @@\n+" + body + "\n"
}
