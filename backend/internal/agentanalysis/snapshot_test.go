package agentanalysis

import (
	"context"
	"errors"
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
		"artifacts/junit.xml": "<failure>timeout</failure>\r\n",
		"build-log.txt":       "start\nrequest failed\n",
		"prowjob.json":        `{"status":"failure"}`,
	}
	firstBrowser := &snapshotBrowser{
		paths: []string{"prowjob.json", "build-log.txt", "artifacts/junit.xml", "build-log.txt", "../unsafe"}, files: files,
	}
	secondBrowser := &snapshotBrowser{
		paths: []string{"artifacts/junit.xml", "build-log.txt", "prowjob.json"}, files: files,
	}
	first, err := FreezeEvidence(t.Context(), firstBrowser, request, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := FreezeEvidence(t.Context(), secondBrowser, request, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash || first.Scan.PathCount != 3 || len(first.Excerpts) != 3 {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	for _, excerpt := range first.Excerpts {
		if strings.Contains(excerpt.Content, "\r") {
			t.Fatalf("excerpt was not normalized: %q", excerpt.Content)
		}
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
