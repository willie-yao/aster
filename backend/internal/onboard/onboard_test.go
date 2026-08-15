package onboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/aster/backend/internal/project"
)

func TestInferCategories_GroupsAndOrders(t *testing.T) {
	jobs := []string{
		"periodic-capz-e2e-aks-main",
		"periodic-capz-e2e-aks-release-1-23",
		"periodic-capz-conformance-main",
		"periodic-capz-conformance-release-1-23",
		"periodic-capz-e2e-main",
		"periodic-capz-e2e-release-1-23",
		"periodic-capz-capi-e2e-main",
		"periodic-capz-capi-e2e-release-1-23",
	}
	rules := InferCategories(jobs)
	if len(rules) == 0 {
		t.Fatal("expected some categories")
	}

	ids := map[string]int{} // id to position
	for i, r := range rules {
		ids[r.ID] = i
		// id and match are the bare token; label is human-cased.
		if r.Match != r.ID {
			t.Errorf("rule %q: match %q != id %q", r.ID, r.Match, r.ID)
		}
	}

	// "aks", "conformance", "capi" each group >=2 jobs; all should appear.
	for _, want := range []string{"aks", "conformance", "capi"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("expected a %q category, got %v", want, ids)
		}
	}
	// Specific categories precede the broad "e2e" category.
	if pos, ok := ids["e2e"]; ok {
		for _, narrow := range []string{"aks", "conformance", "capi"} {
			if ids[narrow] >= pos {
				t.Errorf("expected %q (narrow) before e2e (broad); positions %v", narrow, ids)
			}
		}
	}
}

func TestInferCategories_FiltersNoiseAndUbiquitous(t *testing.T) {
	jobs := []string{
		"periodic-proj-e2e-main",
		"periodic-proj-e2e-release-1-23",
		"periodic-proj-e2e-release-1-24",
	}
	rules := InferCategories(jobs)
	for _, r := range rules {
		switch r.ID {
		case "periodic", "main", "release", "proj", "1", "23", "24":
			t.Errorf("noise/ubiquitous token %q became a category", r.ID)
		}
	}
	// "proj" and "e2e" appear in all jobs, so they are excluded.
}

func TestInferCategories_EdgeCases(t *testing.T) {
	if r := InferCategories(nil); r != nil {
		t.Errorf("nil input: want nil, got %v", r)
	}
	if r := InferCategories([]string{"only-one-job"}); r != nil {
		t.Errorf("single job: want nil, got %v", r)
	}
	// Two identical-shape jobs differing only by version: no distinguishing
	// token, so the result is a flat grid.
	if r := InferCategories([]string{"job-main", "job-release-1-23"}); len(r) != 0 {
		t.Errorf("no distinguisher: want nil, got %v", r)
	}
}

func TestInferCategories_RespectsCap(t *testing.T) {
	var jobs []string
	for i := 0; i < 30; i++ {
		jobs = append(jobs, "periodic-proj-flavor"+string(rune('a'+i))+"-main")
	}
	rules := InferCategories(jobs)
	if len(rules) > maxCategories {
		t.Errorf("got %d categories, want <= %d", len(rules), maxCategories)
	}
}

func TestInferCategories_NeverEmitsReservedOther(t *testing.T) {
	// "other" is reserved fallback id and must never become a category.
	jobs := []string{
		"periodic-proj-other-main", "periodic-proj-other-release-1-23",
		"periodic-proj-foo-main",
	}
	for _, r := range InferCategories(jobs) {
		if r.ID == "other" {
			t.Error("emitted reserved category id \"other\"")
		}
	}
}

func TestInferCategories_SubstringCoverage(t *testing.T) {
	// "capi" contains "api"; coverage must use the engine's substring semantics
	// so the proposed rules validate and classify as they will at runtime.
	jobs := []string{
		"periodic-capi-e2e-main", "periodic-capi-e2e-release-1-23",
	}
	// Both jobs share "capi" and "e2e" as exact tokens but those appear in ALL
	// jobs, so there is no distinguisher. Assert it stays valid and loadable.
	rules := InferCategories(jobs)
	for _, r := range rules {
		if strings.TrimSpace(r.ID) != r.ID || r.ID == "" {
			t.Errorf("bad id %q", r.ID)
		}
	}
}

func TestLabelFor(t *testing.T) {
	cases := map[string]string{
		"aks":          "AKS",
		"e2e":          "E2E",
		"ci":           "CI",
		"dual-stack":   "Dual Stack",
		"machine-pool": "Machine Pool",
		"conformance":  "Conformance",
	}
	for in, want := range cases {
		if got := labelFor(in); got != want {
			t.Errorf("labelFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func testOpts() Options {
	return Options{
		TestGrid:      "my-dashboard",
		DashboardRepo: "my-org/my-proj-aster",
		SourceRepo:    "upstream/my-proj",
		EngineRef:     "main",
	}
}

func TestRenderProjectYAML_ValidatesForTestGrid(t *testing.T) {
	opts := testOpts()
	data := buildScaffoldData(opts, InferCategories([]string{
		"periodic-myproj-e2e-main", "periodic-myproj-conformance-main",
	}))
	yamlText, err := renderProjectYAML(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := validateGeneratedYAML(yamlText); err != nil {
		t.Fatalf("generated yaml failed validation: %v\n---\n%s", err, yamlText)
	}
	for _, want := range []string{
		`dashboard: "my-dashboard"`,
		`provider: gcs`,
		`bucket: "kubernetes-ci-logs"`,
		`base_path: "/my-proj-aster"`,
		`site_url: "https://my-org.github.io/my-proj-aster"`,
		`owner: "upstream"`,
		`name: "my-proj"`,
	} {
		if !strings.Contains(yamlText, want) {
			t.Errorf("project.yaml missing %q\n---\n%s", want, yamlText)
		}
	}
	if !strings.Contains(yamlText, `id: "my-proj"`) {
		t.Errorf("expected id my-proj derived from repo name\n%s", yamlText)
	}
}

func TestRenderProjectYAML_ValidatesForBucketGCSWeb(t *testing.T) {
	opts := Options{
		Bucket:        "istio-prow",
		GCSWebBase:    "https://gcsweb.istio.io/s3",
		ExactJobs:     []string{"periodic-istio-e2e", "periodic-istio-upgrade"},
		DashboardRepo: "me/istio-dash",
		SourceRepo:    "istio/istio",
		EngineRef:     "main",
	}
	data := buildScaffoldData(opts, nil)
	yamlText, err := renderProjectYAML(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if err := validateGeneratedYAML(yamlText); err != nil {
		t.Fatalf("gcsweb yaml failed validation: %v\n---\n%s", err, yamlText)
	}
	for _, want := range []string{
		`source: bucket`,
		`provider: gcsweb`,
		`bucket: "istio-prow"`,
		`base: "https://gcsweb.istio.io/s3"`,
		`exact_jobs:`,
		`- "periodic-istio-e2e"`,
		`- "periodic-istio-upgrade"`,
	} {
		if !strings.Contains(yamlText, want) {
			t.Errorf("bucket yaml missing %q\n---\n%s", want, yamlText)
		}
	}
	if strings.Contains(yamlText, "categories:") {
		t.Errorf("did not expect a categories block\n%s", yamlText)
	}
}

func TestRenderProjectYAML_NoBlankLineRuns(t *testing.T) {
	data := buildScaffoldData(testOpts(), nil)
	yamlText, err := renderProjectYAML(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(yamlText, "\n\n\n") {
		t.Errorf("found a run of blank lines:\n%s", yamlText)
	}
}

func TestValidateOptions(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Options)
		wantErr string
	}{
		{"both selectors", func(o *Options) { o.Bucket = "b" }, "exactly one"},
		{"no selector", func(o *Options) { o.TestGrid = "" }, "exactly one"},
		{"missing dashboard repo", func(o *Options) { o.DashboardRepo = "" }, "dashboard-repo"},
		{"missing source repo", func(o *Options) { o.SourceRepo = "" }, "source-repo"},
		{"bad dashboard repo", func(o *Options) { o.DashboardRepo = "noslash" }, "owner/name"},
		{"trailing slash repo", func(o *Options) { o.DashboardRepo = "owner/" }, "owner/name"},
		{"three-part repo", func(o *Options) { o.SourceRepo = "a/b/c" }, "owner/name"},
		{"gcsweb without bucket", func(o *Options) { o.GCSWebBase = "https://x" }, "gcsweb-base"},
		{"exact job without bucket", func(o *Options) { o.ExactJobs = []string{"periodic-job"} }, "requires --bucket"},
		{"unsafe exact job", func(o *Options) { o.TestGrid = ""; o.Bucket = "b"; o.ExactJobs = []string{"../job"} }, "safe Prow job name"},
		{"duplicate exact job", func(o *Options) { o.TestGrid = ""; o.Bucket = "b"; o.ExactJobs = []string{"job", "job"} }, "duplicates"},
		{"required draft with no-prompt", func(o *Options) { o.NoPrompt = true; o.RequirePromptDraft = true }, "valid only"},
		{"required draft without agent mode", func(o *Options) { o.RequirePromptDraft = true }, "valid only"},
		{"update existing with open PR", func(o *Options) { o.UpdateExisting = true; o.OpenPR = true }, "cannot be combined"},
		{"plan out without dry run", func(o *Options) { o.PlanOut = "plan.json" }, "requires --dry-run"},
		{"plan out with open PR", func(o *Options) { o.DryRun = true; o.PlanOut = "plan.json"; o.OpenPR = true }, "cannot be combined"},
		{"endpoint userinfo", func(o *Options) { o.AIEndpoint = "https://user:fixture-secret@example.test/v1" }, "must not contain credentials"},
		{"endpoint token query", func(o *Options) { o.AIEndpoint = "https://example.test/v1?api_key=fixture-secret" }, "must not contain credential query"},
		{"relative endpoint", func(o *Options) { o.AIEndpoint = "not-a-url" }, "absolute HTTP or HTTPS"},
		{"ftp endpoint", func(o *Options) { o.AIEndpoint = "ftp://example.test/model" }, "absolute HTTP or HTTPS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOpts()
			tc.mutate(&opts)
			err := validateOptions(&opts)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateOptions_DefaultsOutDir(t *testing.T) {
	opts := testOpts()
	if err := validateOptions(&opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.OutDir != "my-proj-aster" {
		t.Errorf("OutDir = %q, want the dashboard repo name", opts.OutDir)
	}
	if opts.EngineRef != "main" {
		t.Errorf("EngineRef = %q, want main", opts.EngineRef)
	}
}

func TestValidateOptions_DeploymentProvider(t *testing.T) {
	t.Run("full provider ok", func(t *testing.T) {
		opts := testOpts()
		opts.AIEndpoint, opts.AIModel = "https://x/chat/completions", "m"
		if err := validateOptions(&opts); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// TestScaffold_LoadsViaLoadDir confirms the rendered scaffold loads with a
// non-empty prompt and valid config.
func TestScaffold_LoadsViaLoadDir(t *testing.T) {
	data := buildScaffoldData(testOpts(), InferCategories([]string{
		"periodic-myproj-e2e-main", "periodic-myproj-e2e-release-1-23",
		"periodic-myproj-conformance-main", "periodic-myproj-conformance-release-1-23",
	}))

	projectYAML, err := renderProjectYAML(data)
	if err != nil {
		t.Fatalf("render project.yaml: %v", err)
	}
	prompt, err := render(systemPromptTmpl, data)
	if err != nil {
		t.Fatalf("render prompt: %v", err)
	}
	dir := t.TempDir()
	files := map[string]string{
		"project.yaml":      projectYAML,
		"prompts/system.md": prompt,
	}
	if err := writeFiles(dir, files, false, false, nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, gotPrompt, err := project.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir rejected the scaffold: %v", err)
	}
	if cfg.ID == "" || cfg.Name == "" {
		t.Errorf("loaded config missing id/name: %+v", cfg)
	}
	if strings.TrimSpace(gotPrompt) == "" {
		t.Error("prompt draft must be non-empty (LoadDir requires it)")
	}

	if err := writeFiles(dir, files, false, false, nil); err == nil {
		t.Error("expected writeFiles to refuse overwriting existing files")
	}
}

func TestScaffold_PagesIncludesProviderSetup(t *testing.T) {
	data := buildScaffoldData(testOpts(), nil)

	deploy, err := render(deployYAMLTmpl, data)
	if err != nil {
		t.Fatalf("render deploy workflow: %v", err)
	}
	for _, want := range []string{"vars.AI_API", "vars.AI_ENDPOINT", "vars.AI_MODEL", "vars.AI_REASONING_EFFORT", "secrets.AI_TOKEN"} {
		if !strings.Contains(deploy, want) {
			t.Errorf("deploy workflow missing %q:\n%s", want, deploy)
		}
	}
	for _, unwanted := range []string{"project_dir:", "builds:", "ai: true", "fetch-timeout:", "EMAIL_SMTP_PASSWORD", "ISSUE_TOKEN", "FIX_TOKEN"} {
		if strings.Contains(deploy, unwanted) {
			t.Errorf("deploy workflow includes advanced default %q:\n%s", unwanted, deploy)
		}
	}

	checklist, err := render(checklistTmpl, checklistData{
		Name:           data.Name,
		DashboardOwner: "my-org",
		DashboardName:  data.DashboardName,
		EngineRef:      data.EngineRef,
		AIEnabled:      true,
		AIAPI:          project.AIAPIChatCompletions,
	})
	if err != nil {
		t.Fatalf("render checklist: %v", err)
	}
	for _, want := range []string{"gh variable set AI_API", "gh variable set AI_ENDPOINT", "gh variable set AI_MODEL", "gh variable set AI_REASONING_EFFORT", "gh secret set AI_TOKEN"} {
		if !strings.Contains(checklist, want) {
			t.Errorf("checklist missing %q:\n%s", want, checklist)
		}
	}
	if strings.Contains(checklist, "is a **stub**") {
		t.Errorf("checklist labels every prompt as a stub:\n%s", checklist)
	}
	for _, unwanted := range []string{"SLACK_WEBHOOK_URL", "EMAIL_SMTP_PASSWORD", "ISSUE_TOKEN", "FIX_TOKEN"} {
		if strings.Contains(deploy+checklist, unwanted) {
			t.Errorf("Pages scaffold includes optional feature %q:\n%s\n%s", unwanted, deploy, checklist)
		}
	}
	projectYAML, err := renderProjectYAML(data)
	if err != nil {
		t.Fatalf("render project config: %v", err)
	}
	if strings.Contains(projectYAML, "notifications:") {
		t.Errorf("minimal project scaffold includes optional notifications:\n%s", projectYAML)
	}
}

// TestValidateOptions_Mode checks the deploy-mode flag defaults to pages and
// rejects an unknown value.
func TestValidateOptions_Mode(t *testing.T) {
	t.Run("defaults to pages", func(t *testing.T) {
		opts := testOpts()
		if err := validateOptions(&opts); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if opts.Mode != modePages {
			t.Errorf("Mode = %q, want %q", opts.Mode, modePages)
		}
	})
	t.Run("k8s accepted", func(t *testing.T) {
		opts := testOpts()
		opts.Mode = modeK8s
		if err := validateOptions(&opts); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("unknown rejected", func(t *testing.T) {
		opts := testOpts()
		opts.Mode = "helmchart"
		if err := validateOptions(&opts); err == nil || !strings.Contains(err.Error(), "--mode") {
			t.Errorf("err = %v, want --mode error", err)
		}
	})
}

// TestScaffold_K8sMode confirms the Kubernetes-native scaffold loads, serves at
// the domain root, seeds the Helm values from the AI env, and emits no Pages
// workflow files.
func TestScaffold_K8sMode(t *testing.T) {
	opts := testOpts()
	opts.Mode = modeK8s
	opts.AIEndpoint = "http://model.ns.svc.cluster.local:8000/v1/chat/completions"
	opts.AIModel = "some-model-id"
	if err := validateOptions(&opts); err != nil {
		t.Fatalf("validate: %v", err)
	}
	data := buildScaffoldData(opts, nil)

	// Kubernetes-native serves at the domain root, not a gh-pages subpath.
	if data.BasePath != "/" {
		t.Errorf("base_path = %q, want /", data.BasePath)
	}

	projectYAML, err := renderProjectYAML(data)
	if err != nil {
		t.Fatalf("render project.yaml: %v", err)
	}
	values, err := render(k8sValuesTmpl, data)
	if err != nil {
		t.Fatalf("render values.yaml: %v", err)
	}
	if !strings.Contains(values, opts.AIEndpoint) || !strings.Contains(values, opts.AIModel) {
		t.Errorf("values.yaml did not seed AI endpoint/model from env:\n%s", values)
	}
	if !strings.Contains(values, "type: ClusterIP") || !strings.Contains(values, "networkPolicy:\n  enabled: false") {
		t.Errorf("values.yaml did not preserve safe network defaults:\n%s", values)
	}
	readme, err := render(k8sDeployReadmeTmpl, data)
	if err != nil {
		t.Fatalf("render deploy README: %v", err)
	}
	// The generated guide runs from the consumer root and names the scaffold
	// explicitly. The namespace and release must remain DNS-1123-safe.
	if !strings.Contains(readme, data.DashboardName) || !strings.Contains(readme, `export PROJECT_DIR="$PWD"`) || !strings.Contains(readme, "--values deploy/values.yaml") {
		t.Errorf("README does not describe the scaffold root %q:\n%s", data.DashboardName, readme)
	}
	if strings.Contains(readme, "../"+data.Name+"/") {
		t.Errorf("README uses the display name %q as a path", data.Name)
	}
	if data.Namespace != "my-proj" {
		t.Errorf("Namespace = %q, want DNS-safe my-proj", data.Namespace)
	}
	prompt, err := render(systemPromptTmpl, data)
	if err != nil {
		t.Fatalf("render prompt: %v", err)
	}

	dir := t.TempDir()
	if err := writeFiles(dir, map[string]string{
		"project.yaml":       projectYAML,
		"prompts/system.md":  prompt,
		"deploy/values.yaml": values,
		"deploy/README.md":   readme,
	}, false, false, nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, gotPrompt, err := project.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir rejected the k8s scaffold: %v", err)
	}
	if cfg.Branding.BasePath != "/" {
		t.Errorf("loaded base_path = %q, want /", cfg.Branding.BasePath)
	}
	if strings.TrimSpace(gotPrompt) == "" {
		t.Error("k8s scaffold still needs a non-empty prompts/system.md")
	}
}

func TestScaffoldGuideUsesModeFile(t *testing.T) {
	if got := scaffoldGuide(modePages); got != "CHECKLIST.md" {
		t.Fatalf("Pages guide = %q, want CHECKLIST.md", got)
	}
	if got := scaffoldGuide(modeK8s); got != "deploy/README.md" {
		t.Fatalf("Kubernetes guide = %q, want deploy/README.md", got)
	}
}

func TestScaffoldPRBodyUsesModeGuide(t *testing.T) {
	pages := scaffoldPRBody("Project", modePages, true)
	if !strings.Contains(pages, "CHECKLIST.md") || strings.Contains(pages, "deploy/README.md") {
		t.Fatalf("Pages PR body uses the wrong guide:\n%s", pages)
	}
	k8s := scaffoldPRBody("Project", modeK8s, true)
	if !strings.Contains(k8s, "deploy/README.md") || strings.Contains(k8s, "CHECKLIST.md") {
		t.Fatalf("Kubernetes PR body uses the wrong guide:\n%s", k8s)
	}
}

func TestScaffold_K8sStaysFocused(t *testing.T) {
	data := buildScaffoldData(testOpts(), nil)
	data.Mode = modeK8s

	values, err := render(k8sValuesTmpl, data)
	if err != nil {
		t.Fatal(err)
	}
	readme, err := render(k8sDeployReadmeTmpl, data)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"mode: watch", "type: inprocess", "imageTag: \"\"", "existingSecret: \"<existing-ai-secret>\"",
		"# schedule:", "# namespace:", "chat:\n    enabled: false", "actions:\n    enabled: false",
		"Active values below are settings a new consumer commonly owns", "No engine source checkout",
		"verified-aster-path", "kubernetes doctor", "--chart-version \"$CHART_VERSION\"", "docs/kubernetes-platform.md",
	} {
		if !strings.Contains(values+readme, want) {
			t.Errorf("Kubernetes scaffold missing %q:\n%s\n%s", want, values, readme)
		}
	}
	for _, unwanted := range []string{"EMAIL_SMTP_PASSWORD", "--set ai.token", "ISSUE_TOKEN", "FIX_TOKEN", "clientSecret:", "sessionKey:", "botToken:"} {
		if strings.Contains(values+readme, unwanted) {
			t.Errorf("Kubernetes scaffold includes optional feature %q:\n%s\n%s", unwanted, values, readme)
		}
	}
}

func TestK8sDeployReadmeGuidesSafeProjectSpecificInstall(t *testing.T) {
	data := buildScaffoldData(testOpts(), nil)
	data.Mode = modeK8s

	readme, err := render(k8sDeployReadmeTmpl, data)
	if err != nil {
		t.Fatalf("render readme: %v", err)
	}

	for _, want := range []string{
		`export ASTER="<verified-aster-path>"`,
		`export CLI_VERSION="<published-engine-tag>"`,
		`export CHART_VERSION="${CLI_VERSION#v}"`,
		`export RELEASE="<application-release-from-platform-handoff>"`,
		`export EXECUTION_NAMESPACE=""`,
		`export PUBLIC_URL=""`,
		`export EXPECTED_JOB="<expected-job-name>"`,
		"export NAMESPACE='my-proj'",
		`"$ASTER" onboard doctor`,
		`"$ASTER" kubernetes doctor`,
		`--action install`,
		`"$ASTER" kubernetes install`,
		`--dry-run`,
		`--action upgrade`,
		`"$ASTER" kubernetes upgrade`,
		`rollback "$RELEASE" "$PRIOR_HELM_REVISION" --wait`,
		`--retry 60`,
		`if [ -n "$PUBLIC_URL" ]`,
		`if [ -n "$EXECUTION_NAMESPACE" ]`,
		`/data/ai_cache.json)" = 404`,
		"docs/kubernetes.md",
		"docs/kubernetes-platform.md",
		"docs/kubernetes-reference.md",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("generated Kubernetes README missing %q:\n%s", want, readme)
		}
	}

	for _, unwanted := range []string{
		`export ENGINE_DIR=`,
		`git clone https://github.com/willie-yao/aster`,
		`make -C "$ENGINE_DIR" build`,
		`kubectl --context "$CONTEXT" create namespace`,
		`create secret generic`,
		`--from-literal`,
		`az afd`,
		`insecure-skip-tls-verify=true`,
		`CLI_ASSET=`,
		`SHA256SUMS`,
		`DOWNLOAD_DIR=`,
		`manifest_ready=`,
		`for _ in`,
	} {
		if strings.Contains(readme, unwanted) {
			t.Errorf("generated Kubernetes README contains duplicated or unsupported guidance %q:\n%s", unwanted, readme)
		}
	}

	installSection := strings.Index(readme, "## Live doctor and guarded install")
	upgradeSection := strings.Index(readme, "## Guarded upgrade")
	if installSection < 0 || upgradeSection < 0 {
		t.Fatalf("generated Kubernetes README lacks lifecycle sections:\n%s", readme)
	}
	installBody := readme[installSection:upgradeSection]
	if doctor, install := strings.Index(installBody, `"$ASTER" kubernetes doctor`), strings.Index(installBody, `"$ASTER" kubernetes install`); doctor < 0 || install < 0 || doctor > install {
		t.Fatalf("live doctor does not precede installation:\n%s", readme)
	}
	upgradeBody := readme[upgradeSection:]
	if doctor, upgrade := strings.Index(upgradeBody, `"$ASTER" kubernetes doctor`), strings.Index(upgradeBody, `"$ASTER" kubernetes upgrade`); doctor < 0 || upgrade < 0 || doctor > upgrade {
		t.Fatalf("upgrade doctor does not precede upgrade:\n%s", readme)
	}
}

func TestK8sDeployReadmeIsProjectAndProviderAgnostic(t *testing.T) {
	data := buildScaffoldData(testOpts(), nil)
	data.Mode = modeK8s
	readme, err := render(k8sDeployReadmeTmpl, data)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"CAPZ", "capz", "cluster-api-provider-azure", "prow-dashboard-demo",
		"<expected-capz-job-name>", "aster kubernetes", "Azure Front Door", "AKS",
	} {
		if strings.Contains(readme, forbidden) {
			t.Errorf("generated Kubernetes README contains project or provider assumption %q:\n%s", forbidden, readme)
		}
	}
	for _, required := range []string{"consumer repository", "expected project job", "<expected-job-name>"} {
		if !strings.Contains(readme, required) {
			t.Errorf("generated Kubernetes README lacks generic term %q:\n%s", required, readme)
		}
	}
}

func TestK8sDeployReadmeUsesTheSameProcessForDifferentProjects(t *testing.T) {
	renderConsumer := func(opts Options) (scaffoldData, string, string) {
		t.Helper()
		opts.Mode = modeK8s
		data := buildScaffoldData(opts, nil)
		readme, err := render(k8sDeployReadmeTmpl, data)
		if err != nil {
			t.Fatal(err)
		}
		values, err := render(k8sValuesTmpl, data)
		if err != nil {
			t.Fatal(err)
		}
		return data, readme, values
	}
	firstOpts := testOpts()
	firstOpts.ID = "sample-one"
	firstOpts.Name = "Sample One"
	firstOpts.DashboardRepo = "example/sample-one-dashboard"
	first, firstReadme, firstValues := renderConsumer(firstOpts)

	secondOpts := testOpts()
	secondOpts.ID = "sample-two"
	secondOpts.Name = "Sample Two"
	secondOpts.DashboardRepo = "example/sample-two-dashboard"
	second, secondReadme, secondValues := renderConsumer(secondOpts)

	normalize := func(readme string, data scaffoldData) string {
		return strings.NewReplacer(
			data.Name, "<project-name>",
			data.DashboardName, "<consumer-repository>",
			"'"+data.Namespace+"'", "'<application-namespace>'",
		).Replace(readme)
	}
	if got, want := normalize(firstReadme, first), normalize(secondReadme, second); got != want {
		t.Fatalf("generated projects received different deployment processes\n--- first ---\n%s\n--- second ---\n%s", got, want)
	}
	if firstValues != secondValues {
		t.Fatalf("project identity changed consumer Helm values\n--- first ---\n%s\n--- second ---\n%s", firstValues, secondValues)
	}
}

func TestWriteKubernetesCleanRoomFixture(t *testing.T) {
	out := os.Getenv("CLEANROOM_FIXTURE_OUT")
	if out == "" {
		t.Skip("CLEANROOM_FIXTURE_OUT is not set")
	}
	disabled := false
	opts := testOpts()
	opts.Mode = modeK8s
	opts.AIEnabled = &disabled
	opts.ID = "sample-dashboard"
	opts.Name = "Sample Dashboard"
	opts.DashboardRepo = "example/sample-dashboard"
	opts.SourceRepo = "example/sample-project"
	data := buildScaffoldData(opts, nil)
	projectYAML, err := renderProjectYAML(data)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := render(systemPromptTmpl, data)
	if err != nil {
		t.Fatal(err)
	}
	values, err := render(k8sValuesTmpl, data)
	if err != nil {
		t.Fatal(err)
	}
	readme, err := render(k8sDeployReadmeTmpl, data)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFiles(out, map[string]string{
		"project.yaml":       projectYAML,
		"prompts/system.md":  prompt,
		"deploy/values.yaml": values,
		"deploy/README.md":   readme,
	}, false, false, nil); err != nil {
		t.Fatal(err)
	}
}

func TestKubernetesCleanRoomScaffoldContract(t *testing.T) {
	data := buildScaffoldData(testOpts(), nil)
	data.Mode = modeK8s
	values, err := render(k8sValuesTmpl, data)
	if err != nil {
		t.Fatalf("render values: %v", err)
	}
	readme, err := render(k8sDeployReadmeTmpl, data)
	if err != nil {
		t.Fatalf("render readme: %v", err)
	}
	prompt, err := render(systemPromptTmpl, data)
	if err != nil {
		t.Fatalf("render prompt: %v", err)
	}
	projectYAML, err := renderProjectYAML(data)
	if err != nil {
		t.Fatalf("render project: %v", err)
	}
	dir := t.TempDir()
	for name, content := range map[string]string{
		"project.yaml":       projectYAML,
		"prompts/system.md":  prompt,
		"deploy/values.yaml": values,
		"deploy/README.md":   readme,
	} {
		if strings.TrimSpace(content) == "" {
			t.Errorf("clean-room scaffold file %s is empty", name)
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Errorf("clean-room scaffold file %s was not created as a regular file", name)
		}
	}
	for _, forbidden := range []string{
		"clientSecret:", "sessionKey:", "botToken:", "--set ai.token",
		"CAPZ", "capz", "cluster-api-provider-azure", "prow-dashboard-demo",
		"<expected-capz-job-name>", "aster kubernetes", "runtime/agent-sandbox", "ENGINE_DIR",
		"insecure-skip-tls-verify=true", "kubectl config set-cluster", "az afd",
	} {
		if strings.Contains(values+readme, forbidden) {
			t.Errorf("clean-room scaffold contains forbidden assumption %q", forbidden)
		}
	}
	for _, required := range []string{
		"verified-aster-path", "kubernetes doctor", "--action install", "--action upgrade",
		"kubernetes install", "kubernetes upgrade", "rollback",
		"docs/kubernetes.md", "docs/kubernetes-platform.md", "docs/kubernetes-reference.md",
	} {
		if !strings.Contains(readme, required) {
			t.Errorf("clean-room scaffold is missing %q", required)
		}
	}
}

func TestK8sDeployReadmePrintsVersionedReferences(t *testing.T) {
	data := buildScaffoldData(testOpts(), nil)
	data.Mode = modeK8s
	data.EngineRef = "main"

	readme, err := render(k8sDeployReadmeTmpl, data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readme, "blob/main/") || strings.Contains(readme, "export ENGINE_REF=") {
		t.Fatalf("generated Kubernetes README follows mutable main:\n%s", readme)
	}
	for _, want := range []string{"for path in", "docs/kubernetes.md", "docs/kubernetes-platform.md", "blob/%s/%s", `"$CLI_VERSION" "$path"`} {
		if !strings.Contains(readme, want) {
			t.Fatalf("generated Kubernetes README lacks versioned reference command %q:\n%s", want, readme)
		}
	}
}

func TestK8sDeployReadmeIsConcise(t *testing.T) {
	data := buildScaffoldData(testOpts(), nil)
	data.Mode = modeK8s
	readme, err := render(k8sDeployReadmeTmpl, data)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(readme, "\n") + 1
	if lines < 120 || lines > 200 {
		t.Fatalf("generated Kubernetes README has %d lines, want 120-200", lines)
	}
}

func TestScaffold_KubernetesDisabledAIOmitsTokenInstruction(t *testing.T) {
	disabled := false
	opts := testOpts()
	opts.Mode = modeK8s
	opts.AIEnabled = &disabled
	data := buildScaffoldData(opts, nil)
	values, err := render(k8sValuesTmpl, data)
	if err != nil {
		t.Fatalf("render values: %v", err)
	}
	if !strings.Contains(values, "enabled: false") {
		t.Fatalf("values did not disable AI:\n%s", values)
	}
	readme, err := render(k8sDeployReadmeTmpl, data)
	if err != nil {
		t.Fatalf("render readme: %v", err)
	}
	for _, unwanted := range []string{"--set ai.token=<token>", "create secret generic", "AI_TOKEN_FILE"} {
		if strings.Contains(readme, unwanted) {
			t.Fatalf("AI-disabled install still contains %q:\n%s", unwanted, readme)
		}
	}
	for _, want := range []string{"AI is disabled", "No provider Secret is required", "ai.existingSecret"} {
		if !strings.Contains(readme, want) {
			t.Fatalf("AI-disabled guidance missing %q:\n%s", want, readme)
		}
	}
}

func TestRenderProjectYAML_QuotesUntrustedDiscoveryValues(t *testing.T) {
	opts := testOpts()
	opts.TestGrid = "dashboard\"\nai:\n  endpoint: injected"
	data := buildScaffoldData(opts, nil)
	yamlText, err := renderProjectYAML(data)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	cfg, err := project.Parse([]byte(yamlText))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, yamlText)
	}
	if cfg.TestGrid.Dashboard != opts.TestGrid {
		t.Fatalf("dashboard = %q, want %q", cfg.TestGrid.Dashboard, opts.TestGrid)
	}
	if cfg.AI != nil {
		t.Fatalf("untrusted dashboard injected AI config: %+v", cfg.AI)
	}
}

func TestChecklist_UsesSelectedAPIAndExplainsDeferredAI(t *testing.T) {
	responses, err := render(checklistTmpl, checklistData{
		Name: "Project", DashboardOwner: "example", DashboardName: "dashboard",
		EngineRef: "main", AIEnabled: true, AIAPI: project.AIAPIResponses,
	})
	if err != nil {
		t.Fatalf("render responses checklist: %v", err)
	}
	if !strings.Contains(responses, "AI_API --body responses") {
		t.Fatalf("responses checklist uses the wrong API:\n%s", responses)
	}

	disabled, err := render(checklistTmpl, checklistData{
		Name: "Project", DashboardOwner: "example", DashboardName: "dashboard",
		EngineRef: "main", AIEnabled: false, AIAPI: project.AIAPIResponses,
	})
	if err != nil {
		t.Fatalf("render disabled checklist: %v", err)
	}
	for _, want := range []string{
		"AI is disabled in the initial workflow",
		"remove `ai: false`",
		"gh variable set AI_API --body responses",
		"gh variable set AI_ENDPOINT",
		"gh variable set AI_MODEL",
		"gh variable set AI_REASONING_EFFORT",
		"gh secret set AI_TOKEN",
	} {
		if !strings.Contains(disabled, want) {
			t.Fatalf("AI-disabled checklist missing %q:\n%s", want, disabled)
		}
	}

	body := scaffoldPRBody("Project", modePages, false)
	if strings.Contains(body, "AI provider variables") || strings.Contains(body, "token") {
		t.Fatalf("AI-disabled PR body requests AI setup:\n%s", body)
	}
}

func TestValidateOptions_CredentialCheckPrecedesAPIValidation(t *testing.T) {
	opts := testOpts()
	opts.NoPrompt = true
	opts.GitHubToken = "fixture-github-token"
	opts.AIAPI = opts.GitHubToken
	err := validateOptions(&opts)
	if err == nil || !strings.Contains(err.Error(), "credential was supplied") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), opts.GitHubToken) {
		t.Fatalf("credential leaked into error: %v", err)
	}
}

func TestValidateAIEndpoint_RejectsCommonCredentialQueryKeys(t *testing.T) {
	for _, endpoint := range []string{
		"https://example.test/v1?x-api-key=secret",
		"https://example.test/v1?subscription-key=secret",
		"https://example.test/v1?X-Amz-Credential=secret",
	} {
		if err := validateAIEndpoint(endpoint); err == nil || !strings.Contains(err.Error(), "credential query") {
			t.Errorf("validateAIEndpoint(%q) error = %v", endpoint, err)
		}
	}
}

func TestValidateOptionsPromptTimeout(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		opts := testOpts()
		if err := validateOptions(&opts); err != nil {
			t.Fatal(err)
		}
		if opts.PromptTimeout != defaultPromptDraftTimeout {
			t.Fatalf("prompt timeout = %s", opts.PromptTimeout)
		}
	})
	for _, test := range []struct {
		name    string
		timeout time.Duration
		wantErr bool
	}{
		{name: "minimum", timeout: time.Minute},
		{name: "slow provider", timeout: 30 * time.Minute},
		{name: "maximum", timeout: 2 * time.Hour},
		{name: "too short", timeout: time.Second, wantErr: true},
		{name: "too long", timeout: 2*time.Hour + time.Second, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := testOpts()
			opts.PromptTimeout = test.timeout
			err := validateOptions(&opts)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSweepConfigPreservesExactBucketJobs(t *testing.T) {
	opts := Options{Bucket: "kubernetes-ci-logs", ExactJobs: []string{"periodic-a", "periodic-b"}}
	cfg := sweepConfig(opts)
	if cfg.EffectiveDiscoverySource() != project.DiscoveryBucket || len(cfg.Discovery.ExactJobs) != 2 || cfg.Discovery.ExactJobs[0] != "periodic-a" || cfg.Discovery.ExactJobs[1] != "periodic-b" {
		t.Fatalf("discovery = %+v", cfg.Discovery)
	}
	opts.ExactJobs[0] = "changed"
	if cfg.Discovery.ExactJobs[0] != "periodic-a" {
		t.Fatal("sweep config retained the caller's mutable exact-job slice")
	}
}

func TestValidateOptionsConsumerOwnedReplacementAndArtifactAccess(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Options)
		want   string
	}{
		{name: "replacement needs update", mutate: func(opts *Options) { opts.ReplaceConsumerOwned = true }, want: "requires --update-existing"},
		{name: "invalid artifact access", mutate: func(opts *Options) { opts.ArtifactAccess = "sometimes" }, want: "--artifact-access"},
		{name: "empty deployment reason", mutate: func(opts *Options) { opts.ModeReasons = []string{" "} }, want: "--deployment-reason"},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := testOpts()
			test.mutate(&opts)
			if err := validateOptions(&opts); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	opts := testOpts()
	opts.UpdateExisting = true
	opts.ReplaceConsumerOwned = true
	opts.ArtifactAccess = artifactAccessAuthenticated
	opts.ModeReasons = []string{"Authenticated artifacts require reviewed access."}
	if err := validateOptions(&opts); err != nil {
		t.Fatal(err)
	}
}
