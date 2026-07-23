package analysisruntime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

func testBundleRequest() ai.FailureAnalysisRequest {
	return ai.FailureAnalysisRequest{
		JobID:       "periodic-job",
		BuildPrefix: "logs/periodic-job/1/",
		Build: models.BuildInfo{
			JobName: "periodic-job", BuildID: "1",
			JUnitURLs: []string{"artifacts/junit.xml"}, RepoRefs: map[string]string{"repo": "sha"},
		},
		TestCase:            models.TestCase{Name: "Test A", Status: "failed", FailureMessage: "boom"},
		ConsecutiveFailures: 3,
	}
}

func writeBundleProject(t *testing.T, endpoint, model string) string {
	t.Helper()
	dir := t.TempDir()
	config := `# removed from the immutable bundle
id: analyzer-test
name: Analyzer Test
testgrid:
  dashboard: analyzer-test
storage:
  provider: local
  base: /fixtures
branding:
  title: Analyzer Test
  base_path: /analyzer-test
  site_url: https://example.invalid/analyzer-test
  source_repo:
    owner: example
    name: project
ai:
  api: responses
  endpoint: ` + endpoint + `
  model: ` + model + `
  tools: [filesystem]
  min_tool_calls: 2
`
	writeBundleTestFile(t, filepath.Join(dir, "project.yaml"), config)
	writeBundleTestFile(t, filepath.Join(dir, "prompts", "system.md"), "Investigate build artifacts.\n")
	writeBundleTestFile(t, filepath.Join(dir, "skills", "z-last.yaml"), `id: z-last
triggers: ["z"]
`)
	writeBundleTestFile(t, filepath.Join(dir, "skills", "a-first.yml"), `id: a-first
triggers: ["a"]
`)
	writeBundleTestFile(t, filepath.Join(dir, "skills", "ignored.txt"), "not a skill\n")
	writeBundleTestFile(t, filepath.Join(dir, "private.env"), "AI_TOKEN=must-not-be-bundled\n")
	return dir
}

func writeBundleTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectBundleRoundTripAndMaterialize(t *testing.T) {
	projectDir := writeBundleProject(t, "https://private-model.example/v1/responses?token=secret", "private-model")
	request := testBundleRequest()
	before := testBundleRequest()
	data, digest, err := BuildProjectBundle(projectDir, "contract-v3", request)
	if err != nil {
		t.Fatal(err)
	}
	second, secondDigest, err := BuildProjectBundle(projectDir, "contract-v3", request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, second) || digest != secondDigest {
		t.Fatal("equivalent project bundles were not deterministic")
	}
	if !reflect.DeepEqual(request, before) {
		t.Fatalf("BuildProjectBundle mutated request: %+v", request)
	}
	text := string(data)
	for _, forbidden := range []string{"private-model.example", `"private-model"`, "token=secret", "AI_TOKEN=", "ignored.txt", "removed from the immutable bundle"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("bundle contains forbidden %q", forbidden)
		}
	}
	bundle, err := DecodeProjectBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Digest != digest || bundle.ContractVersion != "contract-v3" || !reflect.DeepEqual(bundle.Request, request) {
		t.Fatalf("bundle = %+v", bundle)
	}
	paths := make([]string, 0, len(bundle.Files))
	for _, file := range bundle.Files {
		paths = append(paths, file.Path)
	}
	wantPaths := []string{"project.yaml", "prompts/system.md", "skills/a-first.yml", "skills/z-last.yaml"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("bundle paths = %v, want %v", paths, wantPaths)
	}
	if err := VerifyProjectBundleDigest(bundle, digest); err != nil {
		t.Fatal(err)
	}
	materialized, cleanup, err := MaterializeProjectBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == nil {
		t.Fatal("bundle cleanup is nil")
	}
	cfg, err := project.Load(filepath.Join(materialized, "project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AI == nil || cfg.AI.Endpoint != "" || cfg.AI.Model != "" || cfg.AI.API != "" || cfg.AI.EffectiveAgentic().MinToolCalls != 2 {
		t.Fatalf("materialized AI config = %+v", cfg.AI)
	}
	loadedSkills, err := skills.Load(materialized)
	if err != nil {
		t.Fatal(err)
	}
	if len(loadedSkills.Skills()) != 2 {
		t.Fatalf("loaded skills = %d", len(loadedSkills.Skills()))
	}
	cleanup()
	if _, err := os.Stat(materialized); !os.IsNotExist(err) {
		t.Fatalf("materialized bundle still exists: %v", err)
	}
}

func TestProjectBundleIdentityChangesWithInputs(t *testing.T) {
	projectDir := writeBundleProject(t, "https://model.invalid/v1/chat/completions", "model")
	request := testBundleRequest()
	_, base, err := BuildProjectBundle(projectDir, "contract-v3", request)
	if err != nil {
		t.Fatal(err)
	}
	request.TestCase.FailureMessage = "changed"
	_, changedRequest, err := BuildProjectBundle(projectDir, "contract-v3", request)
	if err != nil {
		t.Fatal(err)
	}
	writeBundleTestFile(t, filepath.Join(projectDir, "prompts", "system.md"), "Changed prompt.\n")
	_, changedPrompt, err := BuildProjectBundle(projectDir, "contract-v3", testBundleRequest())
	if err != nil {
		t.Fatal(err)
	}
	writeBundleTestFile(t, filepath.Join(projectDir, "skills", "a-first.yml"), `id: a-first
triggers: ["changed"]
`)
	_, changedSkill, err := BuildProjectBundle(projectDir, "contract-v3", testBundleRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, changedContract, err := BuildProjectBundle(projectDir, "contract-v4", testBundleRequest())
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{
		"request": changedRequest, "prompt": changedPrompt, "skill": changedSkill, "contract": changedContract,
	} {
		if got == base {
			t.Fatalf("%s change kept bundle digest %s", name, got)
		}
	}
}

func TestProjectBundleDropsOptionalCacheSeedWhenCombinedBundleIsTooLarge(t *testing.T) {
	dir := writeBundleProject(t, "https://model.invalid/v1/chat/completions", "model")
	writeBundleTestFile(t, filepath.Join(dir, "prompts", "system.md"), strings.Repeat("p", 70<<10))
	request := testBundleRequest()
	key := FailureCacheKey(request)
	entryData, err := json.Marshal(map[string]string{"value": strings.Repeat("c", 30<<10)})
	if err != nil {
		t.Fatal(err)
	}
	seed := map[string]ai.CacheEntry{key: {Key: key, CreatedAt: time.Now().UTC(), Data: entryData}}
	data, _, err := BuildProjectBundleWithCache(dir, "contract-v4", request, seed)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := DecodeProjectBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.CacheSeed) != 0 {
		t.Fatalf("oversized optional cache seed entries = %d, want 0", len(bundle.CacheSeed))
	}
}

func TestProjectBundleRejectsCredentialsSymlinksAndOversize(t *testing.T) {
	t.Run("headers", func(t *testing.T) {
		dir := writeBundleProject(t, "https://model.invalid/v1/chat/completions", "model")
		path := filepath.Join(dir, "project.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.Replace(data, []byte("  tools:"), []byte("  headers:\n    api-key: secret\n  tools:"), 1)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := BuildProjectBundle(dir, "contract-v3", testBundleRequest()); err == nil || !strings.Contains(err.Error(), "ai.headers") {
			t.Fatalf("BuildProjectBundle error = %v", err)
		}
	})
	t.Run("yaml merge", func(t *testing.T) {
		dir := writeBundleProject(t, "https://model.invalid/v1/chat/completions", "model")
		path := filepath.Join(dir, "project.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = bytes.Replace(data, []byte("ai:\n  api: responses\n  endpoint: https://model.invalid/v1/chat/completions\n  model: model\n"), []byte("ai:\n  <<: {endpoint: 'https://model.invalid/v1/chat/completions?token=secret', model: private-model}\n"), 1)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := BuildProjectBundle(dir, "contract-v3", testBundleRequest()); err == nil || !strings.Contains(err.Error(), "merge keys") {
			t.Fatalf("BuildProjectBundle error = %v", err)
		}
	})
	t.Run("yaml anchor", func(t *testing.T) {
		dir := writeBundleProject(t, "https://model.invalid/v1/chat/completions", "model")
		config := `id: analyzer-test
name: Analyzer Test
testgrid:
  dashboard: analyzer-test
storage:
  provider: local
  base: /fixtures
ai:
  endpoint: &provider https://model.invalid/v1/chat/completions?token=secret
  model: model
  tools: [filesystem]
branding:
  title: *provider
  base_path: /analyzer-test
  site_url: https://example.invalid/analyzer-test
  source_repo:
    owner: example
    name: project
`
		writeBundleTestFile(t, filepath.Join(dir, "project.yaml"), config)
		if _, _, err := BuildProjectBundle(dir, "contract-v3", testBundleRequest()); err == nil || !strings.Contains(err.Error(), "anchors or aliases") {
			t.Fatalf("BuildProjectBundle error = %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir := writeBundleProject(t, "https://model.invalid/v1/chat/completions", "model")
		target := filepath.Join(t.TempDir(), "outside.yaml")
		writeBundleTestFile(t, target, "id: outside\ntriggers: [outside]\n")
		if err := os.Symlink(target, filepath.Join(dir, "skills", "outside.yaml")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := BuildProjectBundle(dir, "contract-v3", testBundleRequest()); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("BuildProjectBundle error = %v", err)
		}
	})
	t.Run("oversize", func(t *testing.T) {
		dir := writeBundleProject(t, "https://model.invalid/v1/chat/completions", "model")
		writeBundleTestFile(t, filepath.Join(dir, "prompts", "system.md"), strings.Repeat("x", MaxProjectBundleBytes))
		if _, _, err := BuildProjectBundle(dir, "contract-v3", testBundleRequest()); err == nil || !strings.Contains(err.Error(), "environment limit") {
			t.Fatalf("BuildProjectBundle error = %v", err)
		}
	})
}

func TestDecodeProjectBundleRejectsUnsanitizedProjectWithValidDigest(t *testing.T) {
	dir := writeBundleProject(t, "https://model.invalid/v1/chat/completions", "model")
	data, _, err := BuildProjectBundle(dir, "contract-v3", testBundleRequest())
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := DecodeProjectBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	for i := range bundle.Files {
		if bundle.Files[i].Path == "project.yaml" {
			bundle.Files[i].Content = strings.Replace(bundle.Files[i].Content, "ai:\n", "ai:\n  endpoint: https://private-model.invalid/v1/chat/completions?token=secret\n  model: private-model\n", 1)
		}
	}
	bundle.Digest, err = projectBundleDigest(bundle)
	if err != nil {
		t.Fatal(err)
	}
	tampered, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeProjectBundle(tampered); err == nil || !strings.Contains(err.Error(), "sanitized v3") {
		t.Fatalf("DecodeProjectBundle error = %v", err)
	}
}

func TestDecodeProjectBundleRejectsMalformedOrTamperedInput(t *testing.T) {
	dir := writeBundleProject(t, "https://model.invalid/v1/chat/completions", "model")
	data, digest, err := BuildProjectBundle(dir, "contract-v3", testBundleRequest())
	if err != nil {
		t.Fatal(err)
	}
	var valid map[string]any
	if err := json.Unmarshal(data, &valid); err != nil {
		t.Fatal(err)
	}
	mutate := func(fn func(map[string]any)) []byte {
		clone := map[string]any{}
		raw, _ := json.Marshal(valid)
		_ = json.Unmarshal(raw, &clone)
		fn(clone)
		out, _ := json.Marshal(clone)
		return out
	}
	cases := map[string][]byte{
		"empty":          nil,
		"malformed":      []byte("not json"),
		"oversized":      bytes.Repeat([]byte("x"), MaxProjectBundleBytes+1),
		"multiple":       append(append([]byte{}, data...), []byte(` {}`)...),
		"unknown":        mutate(func(v map[string]any) { v["unknown"] = true }),
		"tampered":       mutate(func(v map[string]any) { v["contract_version"] = "other" }),
		"bad path":       mutate(func(v map[string]any) { v["files"].([]any)[0].(map[string]any)["path"] = "../project.yaml" }),
		"backslash path": mutate(func(v map[string]any) { v["files"].([]any)[0].(map[string]any)["path"] = `skills\bad.yaml` }),
		"duplicate":      mutate(func(v map[string]any) { v["files"] = append(v["files"].([]any), v["files"].([]any)[0]) }),
		"missing file":   mutate(func(v map[string]any) { v["files"] = v["files"].([]any)[1:] }),
		"unsorted": mutate(func(v map[string]any) {
			files := v["files"].([]any)
			files[0], files[1] = files[1], files[0]
		}),
		"invalid request": mutate(func(v map[string]any) {
			v["request"].(map[string]any)["test_case"].(map[string]any)["status"] = "passed"
		}),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeProjectBundle(raw); err == nil {
				t.Fatalf("DecodeProjectBundle(%s) succeeded", name)
			}
		})
	}
	bundle, err := DecodeProjectBundle(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyProjectBundleDigest(bundle, strings.Repeat("0", len(digest))); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("VerifyProjectBundleDigest error = %v", err)
	}
	if err := VerifyProjectBundleContract(bundle); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("VerifyProjectBundleContract error = %v", err)
	}
}
