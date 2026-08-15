package agentanalysis

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

func testRequest() ai.FailureAnalysisRequest {
	return ai.FailureAnalysisRequest{
		JobID: "periodic::job", BuildPrefix: "logs/job/1/",
		Build: models.BuildInfo{
			BuildID: "1", JobName: "job", JUnitURLs: []string{"private-url"},
			RepoRefs: map[string]string{"example/repo": strings.Repeat("a", 40)},
		},
		TestCase: models.TestCase{
			Name: "TestFailure", Status: "failed", FailureMessage: "failed request", FailureBody: "timeout",
			AISummary: &models.AISummary{Summary: "prior"}, AIAnalysis: &models.AIAnalysis{RootCause: "prior"},
		},
		ProwJob:             &ai.ProwJobContext{Name: " job ", JobType: "periodic"},
		ConsecutiveFailures: 3,
	}
}

func testBundle(t *testing.T) EvidenceBundle {
	t.Helper()
	bundle, err := NewEvidenceBundle(
		testRequest(),
		sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)},
		ArtifactScan{PathCount: 2, Digest: hashString("tree")},
		[]skills.PlannedSkill{{ID: "engine.test", RequiredEvidence: []skills.PlannedEvidenceGroup{{ID: "log", CandidatePaths: []string{"build-log.txt"}}}}},
		[]EvidenceExcerpt{{Path: "build-log.txt", Kind: "tail", Content: "start\r\nfailure text\n"}},
		hashString("skills"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestNewEvidenceBundleCanonicalAndDeterministic(t *testing.T) {
	first := testBundle(t)
	if first.Request.Build.JUnitURLs != nil || first.Request.TestCase.AISummary != nil || first.Request.TestCase.AIAnalysis != nil {
		t.Fatalf("request was not canonical: %+v", first.Request)
	}
	if first.Excerpts[0].Content != "start\nfailure text\n" {
		t.Fatalf("excerpt content = %q", first.Excerpts[0].Content)
	}
	second, err := NewEvidenceBundle(
		testRequest(), first.Source, first.Scan, first.Plan,
		[]EvidenceExcerpt{{Kind: "tail", Path: "build-log.txt", Content: "start\nfailure text\n"}}, first.SkillSetHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash || !reflect.DeepEqual(first, second) {
		t.Fatalf("equivalent bundles differ:\n%+v\n%+v", first, second)
	}
}

func TestNewEvidenceBundleSortsExcerptsBeforeHashing(t *testing.T) {
	request := testRequest()
	source := sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)}
	scan := ArtifactScan{PathCount: 2, Digest: hashString("tree")}
	excerpts := []EvidenceExcerpt{
		{Path: "z.log", Kind: "tail", Content: "z"},
		{Path: "a.log", Kind: "read", Content: "a"},
	}
	first, err := NewEvidenceBundle(request, source, scan, nil, excerpts, hashString("skills"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewEvidenceBundle(request, source, scan, nil, []EvidenceExcerpt{excerpts[1], excerpts[0]}, hashString("skills"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash || !reflect.DeepEqual(first.Excerpts, second.Excerpts) {
		t.Fatalf("permuted excerpts changed identity: %+v %+v", first.Excerpts, second.Excerpts)
	}
}

func TestNewEvidenceBundleRejectsTotalExcerptOverflow(t *testing.T) {
	excerpts := make([]EvidenceExcerpt, 5)
	for i := range excerpts {
		excerpts[i] = EvidenceExcerpt{Path: fmt.Sprintf("logs/%d.log", i), Kind: "tail", Content: strings.Repeat("x", maxExcerptBytes)}
	}
	_, err := NewEvidenceBundle(
		testRequest(), sourceinvestigation.Repository{Owner: "example", Name: "repo", Revision: strings.Repeat("a", 40)},
		ArtifactScan{PathCount: 5}, nil, excerpts, hashString("skills"),
	)
	if !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateEvidenceBundleRejectsTamperingAndCredentials(t *testing.T) {
	bundle := testBundle(t)
	tampered := bundle
	tampered.Excerpts = append([]EvidenceExcerpt(nil), bundle.Excerpts...)
	tampered.Excerpts[0].Content = "changed"
	if err := ValidateEvidenceBundle(tampered); err == nil {
		t.Fatal("tampered content was accepted")
	}
	encoded := mustJSON(t, bundle)
	for _, forbidden := range []string{"private-url", "prior"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("bundle contains %q", forbidden)
		}
	}
}

func TestValidateEvidenceBundleBoundsInputs(t *testing.T) {
	bundle := testBundle(t)
	bundle.Scan.PathCount = maxArtifactPathCount + 1
	if err := ValidateEvidenceBundle(bundle); err == nil {
		t.Fatal("oversized scan was accepted")
	}
	bundle = testBundle(t)
	bundle.Source.Revision = strings.Repeat("A", 40)
	if err := ValidateEvidenceBundle(bundle); err == nil {
		t.Fatal("uppercase source revision was accepted")
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFailureRequestHashUsesCanonicalFailureInput(t *testing.T) {
	first := testRequest()
	second := testRequest()
	second.Build.JUnitURLs = []string{"other-private-url"}
	second.TestCase.AISummary = &models.AISummary{Summary: "different prior output"}
	second.TestCase.AIAnalysis = &models.AIAnalysis{RootCause: "different prior output"}
	if FailureRequestHash(first) != FailureRequestHash(second) {
		t.Fatal("non-authoritative fields changed the request hash")
	}
	second.TestCase.FailureMessage = "different failure"
	if FailureRequestHash(first) == FailureRequestHash(second) {
		t.Fatal("failure evidence did not change the request hash")
	}
}
