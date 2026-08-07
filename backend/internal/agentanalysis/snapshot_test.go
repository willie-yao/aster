package agentanalysis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type snapshotBrowser struct {
	paths     []string
	truncated bool
	listErr   error
	files     map[string]string
}

func (b *snapshotBrowser) BuildRoot() string { return "build" }
func (b *snapshotBrowser) List(context.Context, string) (*artifacts.Listing, error) {
	return nil, errors.New("unused")
}
func (b *snapshotBrowser) ListTree(context.Context, int) ([]string, bool, error) {
	return slices.Clone(b.paths), b.truncated, b.listErr
}
func (b *snapshotBrowser) Read(context.Context, string, int, int) ([]byte, int64, error) {
	return nil, 0, errors.New("unused")
}
func (b *snapshotBrowser) Tail(_ context.Context, path string, lines, maxBytes int) (*artifacts.TailResult, error) {
	content, ok := b.files[path]
	if !ok {
		return nil, errors.New("missing")
	}
	if len(content) > maxBytes {
		content = content[len(content)-maxBytes:]
	}
	return &artifacts.TailResult{FileSize: int64(len(b.files[path])), LinesReturned: min(lines, strings.Count(content, "\n")+1), Content: []byte(content)}, nil
}
func (b *snapshotBrowser) Grep(context.Context, string, *regexp.Regexp, int, int, int, int) (*artifacts.GrepResult, error) {
	return nil, errors.New("unused")
}

func TestFreezeEvidenceIsDeterministicAndBounded(t *testing.T) {
	request := testRequest()
	request.TestCase.JUnitFile = "junit.xml"
	source := sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)}
	files := map[string]string{
		"artifacts/junit.xml":    "<failure>timeout</failure>\r\n",
		"artifacts/repeated.log": "start\nrequest failed\n",
		"build-log.txt":          "start\nrequest failed\n",
		"prowjob.json":           `{"status":"failure"}`,
	}
	firstBrowser := &snapshotBrowser{
		paths: []string{"prowjob.json", "build-log.txt", "artifacts/repeated.log", "artifacts/junit.xml", "build-log.txt", "../unsafe"}, files: files,
	}
	secondBrowser := &snapshotBrowser{
		paths: []string{"artifacts/junit.xml", "artifacts/repeated.log", "build-log.txt", "prowjob.json"}, files: files,
	}
	first, err := FreezeEvidence(t.Context(), firstBrowser, request, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := FreezeEvidence(t.Context(), secondBrowser, request, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash || first.Scan.PathCount != 4 || len(first.Excerpts) != 3 {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	for _, excerpt := range first.Excerpts {
		if strings.Contains(excerpt.Content, "\r") {
			t.Fatalf("excerpt was not normalized: %q", excerpt.Content)
		}
	}
}

func TestFreezeEvidenceBackfillsAfterDuplicateContents(t *testing.T) {
	paths := make([]string, 0, freezeMaxExcerpts+1)
	files := map[string]string{}
	for i := 0; i < freezeMaxExcerpts; i++ {
		artifactPath := fmt.Sprintf("logs/%02d/build-log.txt", i)
		paths = append(paths, artifactPath)
		files[artifactPath] = "same repeated output\n"
	}
	paths = append(paths, "z-unique.log")
	files["z-unique.log"] = "unique failure evidence\n"
	bundle, err := FreezeEvidence(
		t.Context(),
		&snapshotBrowser{paths: paths, files: files},
		testRequest(),
		sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	foundUnique := false
	for _, excerpt := range bundle.Excerpts {
		foundUnique = foundUnique || excerpt.Path == "z-unique.log"
	}
	if len(bundle.Excerpts) != 2 || !foundUnique {
		t.Fatalf("excerpts = %+v", bundle.Excerpts)
	}
}

func TestFreezeEvidenceRetainsSkillEvidenceWithinBudget(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "failure.yaml"), []byte(`id: failure
name: Failure
triggers: ["failed request"]
required_evidence:
- id: controller
  any_of: ["controller.*\\.log$"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := skills.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest()
	request.TestCase.JUnitFile = "junit.xml"
	browser := &snapshotBrowser{
		paths: []string{"junit.xml", "logs/controller-manager.log", "unrelated.log"},
		files: map[string]string{
			"junit.xml":                   strings.Repeat("\"\\\n", freezeExcerptBytes/3),
			"logs/controller-manager.log": strings.Repeat("c", 4<<10),
			"unrelated.log":               strings.Repeat("u", 1024),
		},
	}
	bundle, err := FreezeEvidence(t.Context(), browser, request, sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)}, set)
	if err != nil {
		t.Fatal(err)
	}
	foundController := false
	for _, excerpt := range bundle.Excerpts {
		if excerpt.Path == "logs/controller-manager.log" {
			foundController = excerpt.Content != ""
		}
	}
	if !foundController {
		paths := make([]string, 0, len(bundle.Excerpts))
		for _, excerpt := range bundle.Excerpts {
			paths = append(paths, excerpt.Path)
		}
		t.Fatalf("required controller evidence was not retained: %v", paths)
	}
}

func TestFreezeEvidenceFitsJSONEncodedBundle(t *testing.T) {
	request := testRequest()
	request.TestCase.FailureBody = strings.Repeat("failure\n", 768)
	quoted := strings.Repeat("\"\\\n", freezeExcerptBytes/3)
	browser := &snapshotBrowser{paths: []string{"build-log.txt"}, files: map[string]string{"build-log.txt": quoted}}
	bundle, err := FreezeEvidence(t.Context(), browser, request, sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxBundleBytes || len(bundle.Excerpts) != 1 || !bundle.Excerpts[0].Truncated {
		t.Fatalf("encoded bytes=%d excerpts=%+v", len(encoded), bundle.Excerpts)
	}
	instruction, err := buildInstruction(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(instruction)+len(failureAnalysisSkill) > maxAgentPromptBytes {
		t.Fatalf("composed prompt bytes=%d", len(instruction)+len(failureAnalysisSkill))
	}
}

func TestFitEvidenceBundleHandlesWhitespaceTails(t *testing.T) {
	request := testRequest()
	source := sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)}
	excerpts := []EvidenceExcerpt{
		{Path: "junit.xml", Kind: "tail", Content: strings.Repeat("\"\\\n", 16<<10)},
		{Path: "controller.log", Kind: "tail", Content: "required signal" + strings.Repeat(" ", 33<<10)},
	}
	bundle, ok := fitEvidenceBundle(request, source, ArtifactScan{PathCount: 2}, nil, excerpts, "")
	if !ok || len(bundle.Excerpts) != 2 {
		t.Fatalf("fit result ok=%t excerpts=%d", ok, len(bundle.Excerpts))
	}
	foundSignal := false
	for _, excerpt := range bundle.Excerpts {
		foundSignal = foundSignal || strings.Contains(excerpt.Content, "required signal")
	}
	if !foundSignal {
		t.Fatal("required signal was lost from the fitted suffix")
	}
}

func TestFreezeEvidenceCapsUniqueExcerptContent(t *testing.T) {
	request := testRequest()
	source := sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)}
	largeA := strings.Repeat("a", freezeExcerptBytes)
	largeB := strings.Repeat("b", freezeExcerptBytes)
	small := strings.Repeat("c", 16<<10)
	browser := &snapshotBrowser{
		paths: []string{"a.log", "b.log", "c.log"},
		files: map[string]string{"a.log": largeA, "b.log": largeB, "c.log": small},
	}
	bundle, err := FreezeEvidence(t.Context(), browser, request, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, excerpt := range bundle.Excerpts {
		total += len(excerpt.Content)
	}
	if total > maxExcerptTotalBytes || len(bundle.Excerpts) != 3 {
		t.Fatalf("excerpt count=%d bytes=%d", len(bundle.Excerpts), total)
	}
	instruction, err := buildInstruction(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(instruction)+len(failureAnalysisSkill) > maxAgentPromptBytes {
		t.Fatalf("composed prompt bytes=%d", len(instruction)+len(failureAnalysisSkill))
	}
}

func TestSelectEvidenceCandidatesRanksAbbreviatedFailurePathsBeforeMetadata(t *testing.T) {
	request := testRequest()
	request.TestCase.Name = "Windows volume mount failure"
	got := selectEvidenceCandidates(request, nil, []string{
		"finished.json",
		"logs/controller.log",
		"logs/node-win.log",
	})
	want := []string{"logs/node-win.log", "logs/controller.log", "finished.json"}
	if !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
}

func TestPathMatchesFailureTokenUsesBoundedPrefixMatch(t *testing.T) {
	if !pathMatchesFailureToken("logs/node-win.log", []string{"logs", "node", "win", "log"}, "windows") {
		t.Fatal("windows token did not match node-win path")
	}
	if pathMatchesFailureToken("logs/api.log", []string{"logs", "api", "log"}, "failure") {
		t.Fatal("unrelated short path token matched failure")
	}
}

func TestFreezeEvidenceUsesUnusedBudgetForSingleCandidate(t *testing.T) {
	content := "PRIMARY_FAILURE_MARKER\n" + strings.Repeat("x", 40<<10)
	bundle, err := FreezeEvidence(
		t.Context(),
		&snapshotBrowser{paths: []string{"build-log.txt"}, files: map[string]string{"build-log.txt": content}},
		testRequest(),
		sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Excerpts) != 1 || !strings.Contains(bundle.Excerpts[0].Content, "PRIMARY_FAILURE_MARKER") || len(bundle.Excerpts[0].Content) != len(content) {
		t.Fatalf("excerpt = %+v", bundle.Excerpts)
	}
}

func TestFreezeEvidenceReservesBudgetForLaterCandidates(t *testing.T) {
	paths := make([]string, 0, freezeMaxExcerpts)
	files := map[string]string{}
	for i := 0; i < freezeMaxExcerpts; i++ {
		path := fmt.Sprintf("%02d.log", i)
		paths = append(paths, path)
		files[path] = strings.Repeat(string(rune('a'+i)), freezeExcerptBytes)
	}
	bundle, err := FreezeEvidence(
		t.Context(),
		&snapshotBrowser{paths: paths, files: files},
		testRequest(),
		sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Excerpts) != freezeMaxExcerpts {
		t.Fatalf("excerpt count = %d, want %d", len(bundle.Excerpts), freezeMaxExcerpts)
	}
	total := 0
	for _, excerpt := range bundle.Excerpts {
		total += len(excerpt.Content)
	}
	if total > maxExcerptTotalBytes {
		t.Fatalf("excerpt bytes = %d", total)
	}
}

func TestFreezeEvidenceUsesSkillPlan(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "failure.yaml"), []byte(`id: failure
name: Failure
triggers: ["failed request"]
required_evidence:
- id: controller
  any_of: ["controller.*\\.log$"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := skills.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	browser := &snapshotBrowser{
		paths: []string{"logs/controller-manager.log", "other.txt"},
		files: map[string]string{"logs/controller-manager.log": "request failed\n"},
	}
	bundle, err := FreezeEvidence(t.Context(), browser, testRequest(), sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)}, set)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Plan) != 1 || bundle.Plan[0].RequiredEvidence[0].CandidatePaths[0] != "logs/controller-manager.log" {
		t.Fatalf("plan = %+v", bundle.Plan)
	}
}

func TestFreezeEvidenceRequiresSubstantiveExcerpt(t *testing.T) {
	browser := &snapshotBrowser{paths: []string{"build-log.txt"}, files: map[string]string{}}
	_, err := FreezeEvidence(t.Context(), browser, testRequest(), sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)}, nil)
	if !errors.Is(err, ErrEvidenceUnavailable) {
		t.Fatalf("error = %v", err)
	}
}
