package ai

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/models"
)

const (
	causeProjectOwner = "kubernetes-sigs"
	causeProjectName  = "cluster-api-provider-azure"
)

func TestNormalizeCauseRepositoryAcceptsSupportedReferenceForms(t *testing.T) {
	for _, testCase := range []struct {
		raw  string
		want string
	}{
		{"kubernetes/kubernetes", "kubernetes/kubernetes"},
		{"  kubernetes/kubernetes  ", "kubernetes/kubernetes"},
		{"github.com/kubernetes/kubernetes", "kubernetes/kubernetes"},
		{"https://github.com/kubernetes/kubernetes", "kubernetes/kubernetes"},
		{"https://github.com/kubernetes/kubernetes.git", "kubernetes/kubernetes"},
		{"https://github.com/kubernetes/kubernetes/tree/master/pkg", "kubernetes/kubernetes"},
		{"`kubernetes/kubernetes`", "kubernetes/kubernetes"},
		{"kubernetes", ""},
		{"", ""},
		{"not a repo/at all", ""},
		{"kubernetes/", ""},
		// A repository outside GitHub cannot be addressed or linked, and must
		// never be silently reduced to a host-and-owner pair.
		{"https://gitlab.com/org/repo", ""},
		{"gitlab.com/org/repo", ""},
		{"https://github.example.com/org/repo", ""},
		{"https://owner/repo", ""},
		// A dotted repository name is ordinary; only a dotted leading segment
		// is host-shaped. A Go vanity import is not a GitHub slug, so it is
		// rejected rather than attributed to the wrong owner.
		{"nats-io/nats.go", "nats-io/nats.go"},
		{"https://github.com/nats-io/nats.go", "nats-io/nats.go"},
		{"sigs.k8s.io/controller-runtime", ""},
	} {
		if got := normalizeCauseRepository(testCase.raw); got != testCase.want {
			t.Errorf("normalizeCauseRepository(%q) = %q, want %q", testCase.raw, got, testCase.want)
		}
	}
}

// TestNormalizeCauseLocationMarksDependencyExternal is the shape of the
// verified DRA case: the analyzer names an upstream kubelet file that cannot be
// verified against the project's pinned revision.
func TestNormalizeCauseLocationMarksDependencyExternal(t *testing.T) {
	got := normalizeCauseLocation(&models.AnalysisCauseLocation{
		Repository: "kubernetes/kubernetes",
		Files:      []string{"pkg/kubelet/cm/devicemanager/manager.go"},
	}, causeProjectOwner, causeProjectName, nil)

	if got == nil || !got.External || got.Repository != "kubernetes/kubernetes" {
		t.Fatalf("dependency location = %+v", got)
	}
	if !slices.Equal(got.Files, []string{"pkg/kubelet/cm/devicemanager/manager.go"}) {
		t.Fatalf("dependency file hints = %v", got.Files)
	}
}

// TestNormalizeCauseLocationKeepsProjectCauseInternal is the negative control:
// ownership classification must never make an own-repo cause look foreign.
func TestNormalizeCauseLocationKeepsProjectCauseInternal(t *testing.T) {
	for _, repository := range []string{
		"kubernetes-sigs/cluster-api-provider-azure",
		"KUBERNETES-SIGS/CLUSTER-API-PROVIDER-AZURE",
		"https://github.com/kubernetes-sigs/cluster-api-provider-azure",
	} {
		got := normalizeCauseLocation(&models.AnalysisCauseLocation{Repository: repository}, causeProjectOwner, causeProjectName, nil)
		if got == nil || got.External || got.Repository != causeProjectOwner+"/"+causeProjectName {
			t.Fatalf("project location for %q = %+v", repository, got)
		}
	}
}

func TestNormalizeCauseLocationDropsUnusableOwnership(t *testing.T) {
	valid := &models.AnalysisCauseLocation{Repository: "kubernetes/kubernetes"}
	if got := normalizeCauseLocation(nil, causeProjectOwner, causeProjectName, nil); got != nil {
		t.Fatalf("absent location = %+v", got)
	}
	if got := normalizeCauseLocation(&models.AnalysisCauseLocation{Repository: "kubernetes"}, causeProjectOwner, causeProjectName, nil); got != nil {
		t.Fatalf("unusable repository = %+v", got)
	}
	// Without a configured project repo, "external" cannot be distinguished
	// from the project's own code, so no ownership is claimed.
	if got := normalizeCauseLocation(valid, "", "", nil); got != nil {
		t.Fatalf("unconfigured project location = %+v", got)
	}
}

// TestNormalizeCauseLocationKeepsProjectFilesVerified prevents the location
// field from becoming a weaker second channel for project source paths.
func TestNormalizeCauseLocationKeepsProjectFilesVerified(t *testing.T) {
	got := normalizeCauseLocation(&models.AnalysisCauseLocation{
		Repository: causeProjectOwner + "/" + causeProjectName,
		Files:      []string{"test/e2e/conformance_test.go", "controllers/never_read.go"},
	}, causeProjectOwner, causeProjectName, []string{"test/e2e/conformance_test.go"})

	if got == nil || !slices.Equal(got.Files, []string{"test/e2e/conformance_test.go"}) {
		t.Fatalf("project file hints = %+v", got)
	}
}

func TestNormalizeCauseLocationBoundsAndCleansFileHints(t *testing.T) {
	files := []string{"a.log", "../escape.go", "dup.go", "dup.go", "artifacts/build.go"}
	for index := range maxCauseLocationFiles + 5 {
		files = append(files, "pkg/file"+string(rune('a'+index%26))+"/x.go")
	}
	got := normalizeCauseLocation(&models.AnalysisCauseLocation{Repository: "kubernetes/kubernetes", Files: files},
		causeProjectOwner, causeProjectName, nil)
	if got == nil {
		t.Fatal("location was dropped")
	}
	if len(got.Files) > maxCauseLocationFiles {
		t.Fatalf("file hints exceeded bound: %v", got.Files)
	}
	for _, file := range got.Files {
		if strings.Contains(file, "..") || strings.HasPrefix(file, "artifacts/") || strings.HasSuffix(file, ".log") {
			t.Fatalf("unusable hint survived: %q in %v", file, got.Files)
		}
	}
	if slices.Contains(got.Files[1:], got.Files[0]) {
		t.Fatalf("duplicate hint survived: %v", got.Files)
	}
}

// TestExternalCauseReplacesBoilerplateRemediation covers the specific defect
// from the report: an upstream cause published the generic project-automation
// sentence instead of naming the repository that has to change.
func TestExternalCauseReplacesBoilerplateRemediation(t *testing.T) {
	state := &agentState{sourceOwner: causeProjectOwner, sourceName: causeProjectName}
	parsed := analysisResponse{
		SuggestedFix: "Patch pkg/kubelet/cm/devicemanager/manager.go to skip DRA resources.",
		CauseLocation: &models.AnalysisCauseLocation{
			Repository: "kubernetes/kubernetes",
			Files:      []string{"pkg/kubelet/cm/devicemanager/manager.go"},
		},
	}

	got := state.preparePublishedAnalysis(parsed)
	if got.CauseLocation == nil || !got.CauseLocation.External {
		t.Fatalf("cause location = %+v", got.CauseLocation)
	}
	if !strings.Contains(got.SuggestedFix, "kubernetes/kubernetes") {
		t.Fatalf("remediation %q does not name the owning repository", got.SuggestedFix)
	}
	// The reported path is published as a structured hint, not as prose.
	if !slices.Contains(got.CauseLocation.Files, "pkg/kubelet/cm/devicemanager/manager.go") {
		t.Fatalf("cause location dropped the reported path: %+v", got.CauseLocation)
	}
	// The upstream path is a hint, never a verified project source path.
	if slices.Contains(got.RelevantFiles, "pkg/kubelet/cm/devicemanager/manager.go") {
		t.Fatalf("unverified upstream path entered relevant files: %v", got.RelevantFiles)
	}
}

// TestExternalRemediationKeepsDependencyPathsOutOfProse is the leak guard. The
// remediation fallback is assigned after the ungrounded-path sanitizer runs, so
// a dependency path written into it would skip that gate and then be handed to
// the file-link resolver, which verifies relative paths against the project's
// repository. A path present in both repositories would become a verified
// project link and an actionable source file for the wrong file.
func TestExternalRemediationKeepsDependencyPathsOutOfProse(t *testing.T) {
	state := &agentState{sourceOwner: causeProjectOwner, sourceName: causeProjectName}
	// A path that plausibly exists in both the dependency and the project.
	const colliding = "test/e2e/framework/pod.go"
	got := state.preparePublishedAnalysis(analysisResponse{
		SuggestedFix: "Patch " + colliding + " upstream.",
		CauseLocation: &models.AnalysisCauseLocation{
			Repository: "kubernetes/kubernetes",
			Files:      []string{colliding},
		},
	})

	if got.CauseLocation == nil || !slices.Contains(got.CauseLocation.Files, colliding) {
		t.Fatalf("structured hint was lost: %+v", got.CauseLocation)
	}
	// The published prose the link resolver scans must carry no path token.
	prose := got.RootCause + "\n" + got.SuggestedFix + "\n" + got.Summary
	if matches := pathTokenRe.FindAllString(prose, -1); len(matches) > 0 {
		t.Fatalf("dependency path reached link-resolved prose: %v in %q", matches, prose)
	}
	if !strings.Contains(got.SuggestedFix, "kubernetes/kubernetes") {
		t.Fatalf("remediation dropped the upstream repository: %q", got.SuggestedFix)
	}
	// The repository slug itself must not read as a source path either.
	if sourceCitationRE.MatchString(got.SuggestedFix) {
		t.Fatalf("remediation text parsed as a source citation: %q", got.SuggestedFix)
	}
}

// TestExternalRemediationIsNotRewrittenBySanitizer pins the ordering: the
// fallback is decided from the model's own text but applied after the path
// sanitizer, so its wording is never rewritten on the way out. A repository slug
// that reads as a source path, such as a Go module repository, is the case that
// detects the ordering being reversed.
func TestExternalRemediationIsNotRewrittenBySanitizer(t *testing.T) {
	state := &agentState{sourceOwner: causeProjectOwner, sourceName: causeProjectName}
	got := state.preparePublishedAnalysis(analysisResponse{
		SuggestedFix:  "Patch conn.go upstream.",
		CauseLocation: &models.AnalysisCauseLocation{Repository: "nats-io/nats.go"},
	})
	if got.SuggestedFix != externalRemediationFallback(got.CauseLocation) {
		t.Fatalf("external remediation was rewritten: %q", got.SuggestedFix)
	}
}

// TestProjectCauseKeepsReportedRemediation is the counterpart to the external
// case: a cause owned by this project keeps the instruction its own analysis
// reported instead of a generic sentence, even when the path it names was never
// opened and is therefore stripped from the prose.
func TestProjectCauseKeepsReportedRemediation(t *testing.T) {
	state := &agentState{sourceOwner: causeProjectOwner, sourceName: causeProjectName}
	got := state.preparePublishedAnalysis(analysisResponse{
		SuggestedFix:  "Update controllers/never_read.go to requeue on conflict and rerun.",
		CauseLocation: &models.AnalysisCauseLocation{Repository: causeProjectOwner + "/" + causeProjectName},
	})
	if !strings.Contains(got.SuggestedFix, "requeue on conflict") {
		t.Fatalf("project remediation = %q", got.SuggestedFix)
	}
	if strings.Contains(got.SuggestedFix, "controllers/never_read.go") {
		t.Fatalf("unopened path survived in prose: %q", got.SuggestedFix)
	}
}

func TestMergeCauseLocationsRequiresAgreement(t *testing.T) {
	upstream := func(files ...string) *models.AnalysisCauseLocation {
		return &models.AnalysisCauseLocation{Repository: "kubernetes/kubernetes", External: true, Files: files}
	}
	project := &models.AnalysisCauseLocation{Repository: causeProjectOwner + "/" + causeProjectName}

	merged := MergeCauseLocations([]*models.AnalysisCauseLocation{upstream("a.go"), upstream("b.go"), upstream("a.go")})
	if merged == nil || !merged.External || !slices.Equal(merged.Files, []string{"a.go", "b.go"}) {
		t.Fatalf("agreeing merge = %+v", merged)
	}
	if got := MergeCauseLocations([]*models.AnalysisCauseLocation{upstream(), project}); got != nil {
		t.Fatalf("disagreeing merge = %+v", got)
	}
	if got := MergeCauseLocations([]*models.AnalysisCauseLocation{upstream(), nil}); got != nil {
		t.Fatalf("merge with unattributed member = %+v", got)
	}
	if got := MergeCauseLocations(nil); got != nil {
		t.Fatalf("empty merge = %+v", got)
	}
	if got := MergeCauseLocations([]*models.AnalysisCauseLocation{
		{Repository: "a/b", External: true}, {Repository: "c/d", External: true},
	}); got != nil {
		t.Fatalf("different external repositories merged: %+v", got)
	}
}

func TestGroupCauseLocationRequiresEveryCoveredBuild(t *testing.T) {
	upstream := &models.AnalysisCauseLocation{Repository: "kubernetes/kubernetes", External: true}
	byBuild := map[string]*models.AnalysisCauseLocation{"1": upstream, "2": upstream}

	if got := groupCauseLocation([]string{"1", "2"}, byBuild); got == nil || !got.External {
		t.Fatalf("agreeing group = %+v", got)
	}
	// A build the correlation named but that carries no analysis leaves the
	// group unattributed rather than extrapolating from the rest.
	if got := groupCauseLocation([]string{"1", "2", "3"}, byBuild); got != nil {
		t.Fatalf("group with unanalyzed build = %+v", got)
	}
	if got := groupCauseLocation(nil, byBuild); got != nil {
		t.Fatalf("empty group = %+v", got)
	}
}

func TestBuildPatternAnalysisPublishesAgreedGroupOwnership(t *testing.T) {
	upstream := &models.AnalysisCauseLocation{Repository: "kubernetes/kubernetes", External: true, Files: []string{"pkg/kubelet/cm/devicemanager/manager.go"}}
	failures := []PatternFailure{
		{BuildID: "1", CauseLocation: upstream},
		{BuildID: "2", CauseLocation: upstream},
		{BuildID: "3", CauseLocation: &models.AnalysisCauseLocation{Repository: causeProjectOwner + "/" + causeProjectName}},
	}
	response := patternResponse{Groups: []patternCausalGroup{
		{Builds: []string{"1", "2"}, RootCause: "upstream device plugin", Confidence: "high"},
		{Builds: []string{"2", "3"}, RootCause: "mixed ownership", Confidence: "medium"},
	}}

	pattern := buildPatternAnalysis("subject", len(failures), response, failures, "2026-01-01T00:00:00Z")
	if len(pattern.CausalGroups) != 2 {
		t.Fatalf("groups = %d", len(pattern.CausalGroups))
	}
	agreed := pattern.CausalGroups[0].CauseLocation
	if agreed == nil || !agreed.External || agreed.Repository != "kubernetes/kubernetes" ||
		!slices.Equal(agreed.Files, []string{"pkg/kubelet/cm/devicemanager/manager.go"}) {
		t.Fatalf("agreed group location = %+v", agreed)
	}
	if got := pattern.CausalGroups[1].CauseLocation; got != nil {
		t.Fatalf("mixed group location = %+v", got)
	}
}

// TestCauseLocationSurvivesCacheRoundTrip keeps ownership on a reused entry, so
// a cache hit does not silently fall back to the pre-ownership dead end.
func TestCauseLocationSurvivesCacheRoundTrip(t *testing.T) {
	now := time.Now().UTC()
	const key = "agentic:universal:job:1:failure"
	location := &models.AnalysisCauseLocation{
		Repository: "kubernetes/kubernetes", External: true,
		Files: []string{"pkg/kubelet/cm/devicemanager/manager.go"},
	}
	result := FailureAnalysisResult{
		Summary: &models.AISummary{Summary: "summary"},
		Analysis: &models.AIAnalysis{
			Mode: AgenticMode, RootCause: "root", Severity: "High", SuggestedFix: "fix",
			CauseLocation: location, CritiquePassed: true, CritiqueVersion: currentCritiqueVersion,
		},
	}
	entry, err := NewAgenticCacheEntry(key, result, now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	got, reason := AcceptAgenticCacheEntry(entry, key, AgenticCachePolicy{CritiquePolicy: CritiqueCachePolicyStrict, Now: now})
	if reason != CacheAccepted {
		t.Fatalf("round trip reason = %q", reason)
	}
	if got.Analysis.CauseLocation == nil || !got.Analysis.CauseLocation.External ||
		got.Analysis.CauseLocation.Repository != location.Repository ||
		!slices.Equal(got.Analysis.CauseLocation.Files, location.Files) {
		t.Fatalf("cached cause location = %+v", got.Analysis.CauseLocation)
	}
	// The reconstructed entry must not alias the source slice.
	got.Analysis.CauseLocation.Files[0] = "mutated"
	if location.Files[0] != "pkg/kubelet/cm/devicemanager/manager.go" {
		t.Fatal("cache round trip aliased the caller's file hints")
	}
}

func TestResponseFormatDocumentsCauseOwnership(t *testing.T) {
	for _, required := range []string{"cause_location", "repository", "dependency", "unverified hint"} {
		if !strings.Contains(ResponseFormatFooter, required) {
			t.Fatalf("ResponseFormatFooter missing %q", required)
		}
	}
}

func TestSourceRepoSectionNamesTheProjectRepository(t *testing.T) {
	got := agenticSourceRepoSection(causeProjectOwner, causeProjectName)
	if !strings.Contains(got, causeProjectOwner+"/"+causeProjectName) {
		t.Fatalf("source repo section = %q", got)
	}
	if agenticSourceRepoSection("", causeProjectName) != "" || agenticSourceRepoSection(causeProjectOwner, "") != "" {
		t.Fatal("unconfigured project produced a repository claim")
	}
}

// TestAnalysisPromptHashTracksTheProjectRepository stops a cached ownership
// verdict from outliving the repository it was made against: repointing the
// project's source repo can invert external versus own-repo classification, so
// the entry must be re-analyzed rather than reused.
func TestAnalysisPromptHashTracksTheProjectRepository(t *testing.T) {
	service := NewService(ServiceConfig{Client: &Client{}, Module: &stubModule{name: "kubernetes"}, SystemPrompt: "sys", ConsecutiveFailures: nil})
	unconfigured := service.analysisPromptHash(nil, "")

	service.sourceRepoOwner, service.sourceRepoName = causeProjectOwner, causeProjectName
	configured := service.analysisPromptHash(nil, "")

	service.sourceRepoOwner, service.sourceRepoName = "kubernetes", "kubernetes"
	repointed := service.analysisPromptHash(nil, "")

	if unconfigured == configured || configured == repointed || unconfigured == repointed {
		t.Fatalf("prompt hash ignored the project repository: unconfigured=%s configured=%s repointed=%s",
			unconfigured, configured, repointed)
	}
}

// TestCloneTestCaseCopiesCauseOwnership keeps the shared deep-clone invariant:
// a working copy must not be able to mutate the analysis it was cloned from.
func TestCloneTestCaseCopiesCauseOwnership(t *testing.T) {
	original := models.TestCase{AIAnalysis: &models.AIAnalysis{
		CauseLocation: &models.AnalysisCauseLocation{
			Repository: "kubernetes/kubernetes", External: true,
			Files: []string{"pkg/kubelet/cm/devicemanager/manager.go"},
		},
	}}

	cloned := cloneTestCase(original)
	cloned.AIAnalysis.CauseLocation.Repository = "other/repo"
	cloned.AIAnalysis.CauseLocation.Files[0] = "mutated"

	source := original.AIAnalysis.CauseLocation
	if source.Repository != "kubernetes/kubernetes" || source.Files[0] != "pkg/kubelet/cm/devicemanager/manager.go" {
		t.Fatalf("clone aliased the original cause location: %+v", source)
	}
}

// TestExternalHintCollidingWithVerifiedProjectFileIsNotForeign resolves a
// contradiction the model can produce: a path proven by a project read at the
// pinned revision cannot also be a dependency file. The proven reading wins so
// one path never carries both meanings.
func TestExternalHintCollidingWithVerifiedProjectFileIsNotForeign(t *testing.T) {
	got := normalizeCauseLocation(&models.AnalysisCauseLocation{
		Repository: "kubernetes/kubernetes",
		Files:      []string{"test/e2e/conformance_test.go", "pkg/kubelet/cm/devicemanager/manager.go"},
	}, causeProjectOwner, causeProjectName, []string{"test/e2e/conformance_test.go"})

	if got == nil || !got.External {
		t.Fatalf("dependency ownership was lost: %+v", got)
	}
	if !slices.Equal(got.Files, []string{"pkg/kubelet/cm/devicemanager/manager.go"}) {
		t.Fatalf("verified project path stayed a dependency hint: %v", got.Files)
	}
}
