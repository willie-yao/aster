package onboard

import (
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
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
		DashboardRepo: "my-org/my-proj-prow-ai-dashboard",
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
		`base_path: "/my-proj-prow-ai-dashboard"`,
		`site_url: "https://my-org.github.io/my-proj-prow-ai-dashboard"`,
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
		{"ai token without endpoint or model", func(o *Options) { o.AIToken = "fixture-token" }, "AI_ENDPOINT and AI_MODEL"},
		{"ai token without model", func(o *Options) { o.AIToken = "fixture-token"; o.AIEndpoint = "https://x" }, "AI_ENDPOINT and AI_MODEL"},
		{"required draft with no-prompt", func(o *Options) { o.NoPrompt = true; o.RequirePromptDraft = true }, "valid only"},
		{"required draft without token", func(o *Options) { o.RequirePromptDraft = true }, "AI_TOKEN is required"},
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
	if opts.OutDir != "my-proj-prow-ai-dashboard" {
		t.Errorf("OutDir = %q, want the dashboard repo name", opts.OutDir)
	}
	if opts.EngineRef != "main" {
		t.Errorf("EngineRef = %q, want main", opts.EngineRef)
	}
}

// TestValidateOptions_AIProviderExplicit checks AI drafting requires endpoint
// and model unless -no-prompt is set.
func TestValidateOptions_AIProviderExplicit(t *testing.T) {
	t.Run("full provider ok", func(t *testing.T) {
		opts := testOpts()
		opts.AIToken, opts.AIEndpoint, opts.AIModel = "secret-987654", "https://x/chat/completions", "m"
		if err := validateOptions(&opts); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("no-prompt skips the requirement", func(t *testing.T) {
		opts := testOpts()
		opts.AIToken, opts.NoPrompt = "secret-987654", true
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
	if err := writeFiles(dir, files); err != nil {
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

	if err := writeFiles(dir, files); err == nil {
		t.Error("expected writeFiles to refuse overwriting existing files")
	}
}

func TestScaffold_PagesIncludesProviderSetup(t *testing.T) {
	data := buildScaffoldData(testOpts(), nil)

	deploy, err := render(deployYAMLTmpl, data)
	if err != nil {
		t.Fatalf("render deploy workflow: %v", err)
	}
	for _, want := range []string{"vars.AI_API", "vars.AI_ENDPOINT", "vars.AI_MODEL", "secrets.AI_TOKEN"} {
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
	for _, want := range []string{"gh variable set AI_API", "gh variable set AI_ENDPOINT", "gh variable set AI_MODEL", "gh secret set AI_TOKEN"} {
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
	}); err != nil {
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
		"mode: watch", "type: inprocess", "imageTag: \"\"", "existingSecret: \"my-proj-ai\"",
		"# schedule:", "# namespace:", "chat:\n    enabled: false", "actions:\n    enabled: false",
		"Active values are common settings", "`mode: cron`", "separately installed", "authentication and secure origin",
		"helm show values", "blob/main/deploy/helm/prow-ai-dashboard/values.yaml", "AI Secret", "--chart-version \"$CHART_VERSION\"",
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
		`export ENGINE_DIR="$HOME/src/prow-ai-dashboard"`,
		"export ENGINE_REF='main'",
		`git clone https://github.com/willie-yao/prow-ai-dashboard.git "$ENGINE_DIR"`,
		`if [ -n "$(git -C "$ENGINE_DIR" status --porcelain)" ]; then`,
		`git -C "$ENGINE_DIR" fetch --tags origin "$ENGINE_REF"`,
		`git -C "$ENGINE_DIR" checkout --detach FETCH_HEAD`,
		`printf 'Fetcher ready: %s\n' "$FETCHER"`,
		`make -C "$ENGINE_DIR" build`,
		`export FETCHER="$ENGINE_DIR/bin/fetcher"`,
		"export RELEASE='my-proj'",
		"export NAMESPACE='my-proj'",
		"kubectl config get-contexts",
		"kubectl --context \"$CONTEXT\" get storageclass",
		`"$FETCHER" onboard doctor`,
		`"$FETCHER" kubernetes install`,
		"--dry-run",
		"create namespace \"$NAMESPACE\"",
		"create secret generic 'my-proj-ai'",
		"printf 'AI token: '",
		"IFS= read -r -s AI_TOKEN",
		"printf '%s' \"$AI_TOKEN\" >\"$AI_TOKEN_FILE\"",
		`if [ -s "$AI_TOKEN_FILE" ]; then`,
		"--from-file=AI_TOKEN=\"$AI_TOKEN_FILE\"",
		"AI Secret was not created because the token file is empty.",
		"helm show values",
		"blob/main/deploy/helm/prow-ai-dashboard/values.yaml",
		"get deploy,pod,pvc",
		"app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=worker",
		`logs -f "deployment/$WORKER_DEPLOYMENT"`,
		"app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=server",
		`port-forward "service/$SERVER_SERVICE" 8080:80`,
		`"$FETCHER" kubernetes upgrade`,
		"docs/kubernetes.md",
		"docs/kubernetes-reference.md",
		"docs/orka.md",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("generated Kubernetes README missing %q:\n%s", want, readme)
		}
	}

	beforeLive, _, found := strings.Cut(readme, "## Live setup and install")
	if !found {
		t.Fatalf("generated Kubernetes README has no live setup section:\n%s", readme)
	}
	for _, write := range []string{"create namespace", "create secret generic", "apply -f -"} {
		if strings.Contains(beforeLive, write) {
			t.Errorf("cluster write %q appears before the live setup section:\n%s", write, beforeLive)
		}
	}
	for _, unsafe := range []string{"--from-literal", "AI_TOKEN=<", "AI_TOKEN='"} {
		if strings.Contains(readme, unsafe) {
			t.Errorf("generated Kubernetes README contains unsafe token guidance %q:\n%s", unsafe, readme)
		}
	}
}

func TestK8sDeployReadmeShellQuotesEngineRef(t *testing.T) {
	data := buildScaffoldData(testOpts(), nil)
	data.Mode = modeK8s
	data.EngineRef = "feature/a'b; echo unsafe"

	readme, err := render(k8sDeployReadmeTmpl, data)
	if err != nil {
		t.Fatalf("render readme: %v", err)
	}
	want := `export ENGINE_REF='feature/a'"'"'b; echo unsafe'`
	if !strings.Contains(readme, want) {
		t.Fatalf("generated Kubernetes README did not shell-quote the engine ref:\n%s", readme)
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
	for _, want := range []string{"AI is disabled in the initial scaffold", "ai.enabled=true", "ai.existingSecret"} {
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
	opts.AIToken = "fixture-ai-token"
	opts.AIAPI = opts.AIToken
	err := validateOptions(&opts)
	if err == nil || !strings.Contains(err.Error(), "credential was supplied") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), opts.AIToken) {
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
