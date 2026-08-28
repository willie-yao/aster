package benchmarks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/tools"
	"github.com/willie-yao/aster/backend/internal/ai/tools/filesystem"
	"github.com/willie-yao/aster/backend/internal/analysischat"
	"github.com/willie-yao/aster/backend/internal/analysisruntime"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/storage"
)

const flatcarCauseResolutionFixture = "testdata/analysis-chat/flatcar-v1358-withdrawn"

var resolvedClaimRE = regexp.MustCompile(`(?i)\b(?:is|was|has been|appears)\s+(?:now\s+)?(?:fixed|resolved)\b`)

type causeResolutionFixtureManifest struct {
	Version      int    `json:"version"`
	Job          string `json:"job"`
	SourceCommit string `json:"source_commit"`
	Expected     string `json:"expected"`
	Builds       map[string]struct {
		Role string `json:"role"`
	} `json:"builds"`
	Files map[string]struct {
		SHA256 string `json:"sha256"`
		Bytes  int    `json:"bytes"`
	} `json:"files"`
}

type causeResolutionBrowserFactory struct {
	base *artifacts.BackendFactory
}

func (f causeResolutionBrowserFactory) ForBuild(prefix, displayName string) artifacts.Browser {
	return f.base.ForBuild(prefix, displayName)
}

func (f causeResolutionBrowserFactory) ForBuilds(builds []analysischat.ArtifactBuild) artifacts.Browser {
	return analysisruntime.NewPatternBrowser(f.base, builds)
}

func TestFlatcarCauseResolutionFixture(t *testing.T) {
	manifest := loadCauseResolutionFixtureManifest(t)
	if manifest.Version != 1 || manifest.Job != "periodic-cluster-api-provider-azure-e2e-v1beta1-release-1-25" ||
		manifest.SourceCommit != "5eafa96647f647c297574725654fe79fda1bc312" {
		t.Fatalf("fixture identity = %+v", manifest)
	}
	roles := map[string]string{
		"2090605942025490432": "failed-member",
		"2091715517269151744": "failed-member",
		"2092830007742173184": "comparison",
	}
	for buildID, role := range roles {
		if manifest.Builds[buildID].Role != role {
			t.Fatalf("build %s role = %q", buildID, manifest.Builds[buildID].Role)
		}
	}
	actual := map[string]struct{}{}
	if err := filepath.WalkDir(flatcarCauseResolutionFixture, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || path == filepath.Join(flatcarCauseResolutionFixture, "manifest.json") {
			return nil
		}
		relative, err := filepath.Rel(flatcarCauseResolutionFixture, path)
		if err != nil {
			return err
		}
		actual[filepath.ToSlash(relative)] = struct{}{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(actual) != len(manifest.Files) {
		t.Fatalf("fixture file set differs from manifest: actual=%d manifest=%d", len(actual), len(manifest.Files))
	}
	for name := range actual {
		if _, ok := manifest.Files[name]; !ok {
			t.Fatalf("fixture file %s is not listed in the manifest", name)
		}
	}
	for name, want := range manifest.Files {
		data, err := os.ReadFile(filepath.Join(flatcarCauseResolutionFixture, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); got != want.SHA256 || len(data) != want.Bytes {
			t.Fatalf("fixture %s changed: sha256=%s bytes=%d", name, got, len(data))
		}
		if strings.Contains(string(data), "PRIVATE KEY") {
			t.Fatalf("fixture %s contains key material", name)
		}
	}
	all := fixtureText(t, manifest)
	for _, want := range []string{
		"stable-1.35) to v1.35.8", "kubernetes-v1.35.8-x86-64.raw", "GET result: Not Found",
		"stable-1.35) to v1.35.6", "kubernetes-v1.35.6-x86-64.raw", "GET result: OK",
		`status="passed"`, `status="failed"`,
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("fixture lacks %q", want)
		}
	}
}

func TestAnalysisChatCauseResolutionBenchmark(t *testing.T) {
	if strings.TrimSpace(os.Getenv("RUN_CAUSE_RESOLUTION_BENCHMARK")) != "1" {
		t.Skip("set RUN_CAUSE_RESOLUTION_BENCHMARK=1 with AI_ENDPOINT and AI_MODEL")
	}
	endpoint, model := strings.TrimSpace(os.Getenv("AI_ENDPOINT")), strings.TrimSpace(os.Getenv("AI_MODEL"))
	if endpoint == "" || model == "" {
		t.Fatal("RUN_CAUSE_RESOLUTION_BENCHMARK requires AI_ENDPOINT and AI_MODEL")
	}
	apiMode, err := benchmarkAPIMode()
	if err != nil {
		t.Fatal(err)
	}
	backend, err := storage.NewLocalBackend(flatcarCauseResolutionFixture, "")
	if err != nil {
		t.Fatal(err)
	}
	factory := causeResolutionBrowserFactory{base: artifacts.NewBackendFactory(backend, "flatcar cause resolution fixture")}
	registry := tools.NewRegistry()
	filesystem.Register(registry)
	enabled, err := registry.Enable([]string{"filesystem"})
	if err != nil {
		t.Fatal(err)
	}
	client := ai.NewClientWithOptions(ai.Options{
		Token: os.Getenv("AI_TOKEN"), API: apiMode, Endpoint: endpoint, Model: model,
	})
	agent, err := ai.NewAnalysisChatAgent(
		client,
		ai.ComposeAnalysisChatSystemPrompt("CAPZ tests resolve stable Kubernetes labels from available community gallery images. Treat a passing run as evidence to compare, not as proof of a fix."),
		registry,
		enabled,
		factory,
		ai.AnalysisChatOptions{MaxIters: 8, ModelByteBudget: 300_000, GCSByteBudget: 16 << 20, ContextByteBudget: 256 << 10, Timeout: 10 * time.Minute},
	)
	if err != nil {
		t.Fatal(err)
	}

	const (
		jobID    = "periodic-cluster-api-provider-azure-e2e-v1beta1-release-1-25"
		testName = "[It] Workload cluster creation Creating a Flatcar sysext cluster [OPTIONAL] With Flatcar control-plane and worker nodes"
	)
	pattern := &models.PatternAnalysis{
		ID: "flatcar-v1358-withdrawn", JobID: jobID, Subject: testName, BuildsAnalyzed: 2, Systemic: true, Confidence: "high",
		Recurrence: models.PatternRecurrenceSharedCause,
		CausalGroups: []models.PatternCausalGroup{{
			ID: "flatcar-v1358-missing", Builds: []string{"2090605942025490432", "2091715517269151744"},
			RootCause: "stable-1.35 resolved to v1.35.8, but the Flatcar kubernetes-v1.35.8-x86-64.raw sysext was unavailable and Ignition received Not Found.", Confidence: "high",
		}},
		SharedRootCause: "The v1.35.8 Flatcar Kubernetes sysext was unavailable.",
		SharedBuilds:    []string{"2090605942025490432", "2091715517269151744"},
		Lifecycle: &models.PatternLifecycle{
			State: models.PatternLifecycleActive, Reason: "One later completed run passed; recovery is not source-verified.",
			RecoveryStreak: 1, RecoveryBuilds: []string{"2092830007742173184"},
		},
		Summary: "Two Flatcar v1.35.8 failures followed by one v1.35.6 pass.",
	}
	turn := analysischat.Turn{
		Scope: analysischat.ScopeCause, JobID: jobID,
		BuildPrefix: "2091715517269151744/",
		Build: models.BuildInfo{
			BuildID: "2091715517269151744", JobName: "periodic-cluster-api-provider-azure-e2e-v1beta1-release-1-25",
			Result: "FAILURE", Commit: "5eafa96647f647c297574725654fe79fda1bc312",
		},
		TestCase: models.TestCase{Name: testName, Status: "failed", AIAnalysis: &models.AIAnalysis{
			RootCause: pattern.SharedRootCause, Severity: "High",
		}},
		Pattern: pattern,
		EvidenceBuilds: []analysischat.ArtifactBuild{
			{BuildPrefix: "2091715517269151744/", Build: models.BuildInfo{BuildID: "2091715517269151744", JobName: pattern.JobID}},
			{BuildPrefix: "2090605942025490432/", Build: models.BuildInfo{BuildID: "2090605942025490432", JobName: pattern.JobID}},
		},
		Comparison: &analysischat.CauseComparison{
			ArtifactBuild: analysischat.ArtifactBuild{
				BuildPrefix: "2092830007742173184/",
				Build: models.BuildInfo{
					BuildID: "2092830007742173184", JobName: pattern.JobID, Result: "SUCCESS", Passed: true,
					Started: time.Date(2026, time.August, 27, 4, 22, 14, 0, time.UTC), Commit: "5eafa96647f647c297574725654fe79fda1bc312",
				},
			},
			TestNames: []string{testName},
		},
		Question: "Has this cause been resolved in the latest completed run?",
	}
	reply, err := agent.Reply(context.Background(), turn)
	if err != nil {
		t.Fatal(err)
	}
	answer := strings.ToLower(reply.Answer)
	for _, want := range []string{"v1.35.8", "v1.35.6"} {
		if !strings.Contains(answer, want) {
			t.Fatalf("answer lacks %q: %s", want, reply.Answer)
		}
	}
	if !containsAny(answer, "not reproduced", "trigger changed", "version changed", "resolved to v1.35.6") {
		t.Fatalf("answer does not identify the changed trigger: %s", reply.Answer)
	}
	if !containsAny(answer, "not fixed", "not been fixed", "still unavailable", "still missing", "not proven") {
		t.Fatalf("answer does not reject a fixed conclusion: %s", reply.Answer)
	}
	if resolvedClaimRE.MatchString(reply.Answer) {
		t.Fatalf("answer falsely claims resolution: %s", reply.Answer)
	}
	failedCitation, comparisonCitation := false, false
	for _, citation := range reply.Citations {
		failedCitation = failedCitation || strings.HasPrefix(citation.Path, "builds/2090605942025490432/") || strings.HasPrefix(citation.Path, "builds/2091715517269151744/")
		comparisonCitation = comparisonCitation || strings.HasPrefix(citation.Path, "builds/2092830007742173184/")
	}
	if !failedCitation || !comparisonCitation {
		t.Fatalf("citations do not cover failed and comparison builds: %+v", reply.Citations)
	}
}

func loadCauseResolutionFixtureManifest(t *testing.T) causeResolutionFixtureManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(flatcarCauseResolutionFixture, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest causeResolutionFixtureManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func fixtureText(t *testing.T, manifest causeResolutionFixtureManifest) string {
	t.Helper()
	names := make([]string, 0, len(manifest.Files))
	for name := range manifest.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	var out strings.Builder
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(flatcarCauseResolutionFixture, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		out.Write(data)
		out.WriteByte('\n')
	}
	return out.String()
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
