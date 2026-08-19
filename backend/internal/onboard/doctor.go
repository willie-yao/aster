package onboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/project"
	"github.com/willie-yao/aster/backend/internal/textutil"
	"gopkg.in/yaml.v3"
)

// DoctorOptions selects an existing consumer scaffold to validate.
type DoctorOptions struct {
	ProjectDir string
}

// DoctorStatus is the outcome of one validation check.
type DoctorStatus string

const (
	DoctorPass DoctorStatus = "pass"
	DoctorWarn DoctorStatus = "warn"
	DoctorFail DoctorStatus = "fail"
)

// DoctorCheck is one actionable validation result.
type DoctorCheck struct {
	Name   string       `json:"name"`
	Status DoctorStatus `json:"status"`
	Detail string       `json:"detail"`
	Action string       `json:"action,omitempty"`
}

// DoctorReport contains every static and discovery check.
type DoctorReport struct {
	ProjectDir string        `json:"project_dir"`
	Checks     []DoctorCheck `json:"checks"`
}

// HasFailures reports whether any doctor check failed.
func (r DoctorReport) HasFailures() bool {
	for _, check := range r.Checks {
		if check.Status == DoctorFail {
			return true
		}
	}
	return false
}

type doctorFileSystem interface {
	ReadFile(string) ([]byte, error)
}

type osDoctorFileSystem struct{}

func (osDoctorFileSystem) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

type doctorDependencies struct {
	files   doctorFileSystem
	sweeper jobSweeper
}

// Doctor validates an existing consumer without mutating files or external systems.
func Doctor(ctx context.Context, opts DoctorOptions) DoctorReport {
	return runDoctor(ctx, opts, doctorDependencies{files: osDoctorFileSystem{}, sweeper: defaultSweeper{}})
}

func normalizeDoctorProjectDir(projectDir string) string {
	dir := strings.TrimSpace(projectDir)
	if dir == "" {
		dir = "."
	}
	if absolute, err := filepath.Abs(dir); err == nil {
		return filepath.Clean(absolute)
	}
	return filepath.Clean(dir)
}

func runDoctor(ctx context.Context, opts DoctorOptions, deps doctorDependencies) DoctorReport {
	dir := normalizeDoctorProjectDir(opts.ProjectDir)
	report := DoctorReport{ProjectDir: dir}
	add := func(name string, status DoctorStatus, detail, action string) {
		report.Checks = append(report.Checks, DoctorCheck{Name: name, Status: status, Detail: detail, Action: action})
	}

	projectPath := filepath.Join(dir, "project.yaml")
	projectYAML, err := deps.files.ReadFile(projectPath)
	if err != nil {
		add("project.yaml", DoctorFail, fmt.Sprintf("cannot read %s", projectPath), "Run fetcher onboard or restore project.yaml, then rerun doctor.")
		return report
	}
	cfg, err := project.Parse(projectYAML)
	if err != nil {
		add("project.yaml", DoctorFail, err.Error(), "Fix the reported project.yaml fields until the strict project loader accepts the file.")
		return report
	}
	add("project.yaml", DoctorPass, "strict project configuration validation passed", "")

	promptPath := filepath.Join(dir, "prompts", "system.md")
	prompt, err := deps.files.ReadFile(promptPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		add("prompts/system.md", DoctorFail, "the required project prompt is missing", "Create a non-empty prompts/system.md and review its project-specific claims.")
	case err != nil:
		add("prompts/system.md", DoctorFail, fmt.Sprintf("cannot read %s: %v", promptPath, err), "Fix prompt file permissions or the read error, then rerun doctor.")
	case strings.TrimSpace(string(prompt)) == "":
		add("prompts/system.md", DoctorFail, "the required project prompt is empty", "Add project-specific prompt content and rerun doctor.")
	default:
		add("prompts/system.md", DoctorPass, "required project prompt is present", "")
	}

	pagesPath, pages, pagesErr := findPagesWorkflow(deps.files, dir)
	k8sPath := filepath.Join(dir, "deploy", "values.yaml")
	k8s, k8sErr := deps.files.ReadFile(k8sPath)
	pagesExists := pagesErr == nil
	k8sExists := k8sErr == nil
	if pagesErr != nil && !errors.Is(pagesErr, os.ErrNotExist) {
		add("deployment", DoctorFail, fmt.Sprintf("cannot read %s", pagesPath), "Fix file permissions and rerun doctor.")
	}
	if k8sErr != nil && !errors.Is(k8sErr, os.ErrNotExist) {
		add("deployment", DoctorFail, fmt.Sprintf("cannot read %s", k8sPath), "Fix file permissions and rerun doctor.")
	}
	var profile doctorProfile
	switch {
	case pagesExists && k8sExists:
		add("deployment", DoctorFail, "both Pages and Kubernetes deployment files are present", "Keep one first-run deployment profile and remove the unintended scaffold files.")
	case pagesExists:
		add("deployment", DoctorPass, "GitHub Pages profile detected", "")
		profile = checkPages(&report, pagesPath, dir, pages, cfg)
	case k8sExists:
		add("deployment", DoctorPass, "Kubernetes with Helm profile detected", "")
		profile = checkKubernetes(&report, k8s, cfg)
	default:
		add("deployment", DoctorFail, "no supported deployment scaffold was found", "Restore .github/workflows/deploy.yml or deploy/values.yaml.")
	}
	profile.includePresubmits = profile.includePresubmits || cfg.Source.IncludePresubmits

	checkPullRequestTriage(add, cfg, profile)

	discoveryCtx, cancel := context.WithTimeout(ctx, onboardingDiscoveryTimeout)
	sweep, err := deps.sweeper.Discover(discoveryCtx, cfg, profile.includePresubmits)
	cancel()
	if err != nil {
		action := "Verify the TestGrid dashboard or artifact bucket, GitHub access, and network connectivity, then rerun doctor."
		add("Prow discovery", DoctorFail, err.Error(), action)
	} else if len(sweep.Jobs) == 0 {
		add("Prow discovery", DoctorFail, "the real discovery sweep found zero jobs", "Correct testgrid.dashboard or discovery/storage settings until at least one job is found.")
	} else {
		add("Prow discovery", DoctorPass, fmt.Sprintf("the real discovery sweep found %d job(s)", len(sweep.Jobs)), "")
	}
	return report
}

// doctorReadTokenSource is how a deployment supplies the read-only GitHub token
// that pull request triage needs.
type doctorReadTokenSource int

const (
	// readTokenUnknown means no deployment profile was inspected, or the profile
	// runs no fetch, so doctor has nothing to say about the credential.
	readTokenUnknown doctorReadTokenSource = iota
	// readTokenAbsent means no deployment setting supplies the token, so triage
	// reads GitHub anonymously.
	readTokenAbsent
	// readTokenShadowed means the deployment supplies the token and then blanks
	// it with a fetcher.extraEnv entry that wins on ordering.
	readTokenShadowed
	// readTokenConfigured means the deployment names the token explicitly.
	readTokenConfigured
	// readTokenOptionalSecret means the token can only come from a Secret key
	// mounted with optional: true, so an absent key is silently ignored.
	readTokenOptionalSecret
)

// doctorProfile is the deployment-derived state that profile checks hand to the
// shared checks running after them.
type doctorProfile struct {
	includePresubmits bool
	readToken         doctorReadTokenSource
}

// checkPullRequestTriage reports how the consumer's settings relate to the pull
// request triage view. Triage resolves presubmits from the job catalog, so
// source.include_presubmits neither enables it nor improves its verdicts, while
// enlarging the analyzed job set.
func checkPullRequestTriage(add func(string, DoctorStatus, string, string), cfg *project.Config, profile doctorProfile) {
	if cfg.PullRequests == nil {
		// An explicit enabled: false is a decision, so only an absent block hints.
		add("pull request triage", DoctorPass, "the optional pull request triage view is not configured",
			"Set pull_requests.enabled: true to triage open pull requests of branding.source_repo. Its attribution verdicts are deterministic and cost no model calls.")
	}
	if cfg.PullRequests != nil && cfg.PullRequests.Enabled {
		checkPullRequestReadToken(add, profile.readToken)
	}
	checkPullRequestComment(add, cfg)
	if profile.includePresubmits {
		add("source.include_presubmits", DoctorWarn, "presubmits join the dashboard job set, enlarging each fetch and any enabled analysis",
			"Keep this on only if you want presubmit rows in the main dashboard. It is not required for pull request triage, which resolves presubmits from the job catalog either way.")
	}
}

// checkPullRequestComment reports the state of the engine's only unattended
// write that contacts a contributor's pull request. A live comment pass writes
// to contributor threads without a maintainer confirming each one, so leaving
// dry run is called out as the deliberate step it is rather than passing
// silently.
func checkPullRequestComment(add func(string, DoctorStatus, string, string), cfg *project.Config) {
	const name = "pull request comments"
	if !cfg.CommentEnabled() {
		add(name, DoctorPass, "the bot comment on new pull requests is disabled", "")
		return
	}
	if cfg.CommentDryRun() {
		add(name, DoctorPass, "commenting is enabled in dry run, so bodies are logged and nothing is posted",
			"Read a real pass's logged bodies before setting pull_requests.comment.dry_run: false.")
		return
	}
	add(name, DoctorWarn, "commenting will post to every newly opened pull request without a maintainer confirming each one",
		"Confirm you have reviewed a dry run's output, that ASTER_APP_ID and ASTER_APP_PRIVATE_KEY name a bot App, and that the App is installed on branding.source_repo.")
}

// checkPullRequestReadToken reports whether triage will read GitHub
// authenticated. Anonymous reads are capped at 60 requests per hour, which one
// pass over a busy repository exhausts, and the 403s that follow only surface
// as a triage view that stops updating.
func checkPullRequestReadToken(add func(string, DoctorStatus, string, string), source doctorReadTokenSource) {
	const name = "pull request triage credential"
	switch source {
	case readTokenUnknown:
		// No deployment profile was inspected, or it runs no fetch at all.
	case readTokenConfigured:
		add(name, DoctorPass, "the deployment supplies a read-only GitHub token", "")
	case readTokenOptionalSecret:
		add(name, DoctorWarn, "the read token comes from a Secret key mounted as optional, so a missing key silently falls back to anonymous reads",
			"Confirm that Secret carries the key, or set ai.githubReadToken or ai.githubReadTokenSecretName so a missing key cannot pass unnoticed.")
	case readTokenShadowed:
		add(name, DoctorWarn, "a fetcher.extraEnv entry blanks the read token the chart renders, and extraEnv is appended last",
			"Remove the empty GITHUB_READ_TOKEN or GITHUB_TOKEN entry from fetcher.extraEnv, or give it a value.")
	default:
		add(name, DoctorWarn, "pull request triage is enabled with no read-only GitHub token, so it reads GitHub anonymously at 60 requests per hour",
			"Set ai.githubReadToken or ai.githubReadTokenSecretName so the fetcher receives GITHUB_READ_TOKEN. A token with no repository privileges is enough for a public source_repo.")
	}
}

func findPagesWorkflow(files doctorFileSystem, projectDir string) (string, []byte, error) {
	current := filepath.Clean(projectDir)
	for {
		path := filepath.Join(current, ".github", "workflows", "deploy.yml")
		data, err := files.ReadFile(path)
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return path, data, err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Join(projectDir, ".github", "workflows", "deploy.yml"), nil, os.ErrNotExist
		}
		current = parent
	}
}

func yamlBool(value any, defaultValue bool) (bool, bool) {
	if value == nil {
		return defaultValue, true
	}
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		if strings.EqualFold(strings.TrimSpace(typed), "true") {
			return true, true
		}
		if strings.EqualFold(strings.TrimSpace(typed), "false") {
			return false, true
		}
	}
	return false, false
}

func checkPages(report *DoctorReport, workflowPath, projectDir string, workflowYAML []byte, cfg *project.Config) (profile doctorProfile) {
	add := func(name string, status DoctorStatus, detail, action string) {
		report.Checks = append(report.Checks, DoctorCheck{Name: name, Status: status, Detail: detail, Action: action})
	}
	type workflowJob struct {
		Uses    string         `yaml:"uses"`
		With    map[string]any `yaml:"with"`
		Secrets map[string]any `yaml:"secrets"`
	}
	var workflow struct {
		Jobs map[string]workflowJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflowYAML, &workflow); err != nil {
		add("Pages workflow", DoctorFail, err.Error(), "Fix .github/workflows/deploy.yml so it is valid YAML.")
		return
	}
	deploy, ok := workflow.Jobs["deploy"]
	if !ok {
		add("Pages workflow", DoctorFail, "jobs.deploy is missing", "Restore the generated deploy job that calls the reusable dashboard workflow.")
		return
	}
	if !reusableDeployReference(deploy.Uses) {
		add("Pages workflow", DoctorFail, "jobs.deploy.uses does not target the dashboard reusable-deploy workflow", "Restore the generated uses target for aster/.github/workflows/reusable-deploy.yml.")
		return
	}
	// The reusable workflow passes the Actions GITHUB_TOKEN to the fetch step
	// unconditionally, so a deploy job that reaches it reads GitHub
	// authenticated regardless of the inputs it sets.
	profile.readToken = readTokenConfigured
	if value, ok := deploy.With["include-presubmits"]; ok {
		if dynamicExpression(value) {
			add("Pages presubmits", DoctorWarn, "include-presubmits is dynamic and cannot be resolved offline", "Confirm the expression enables presubmits when the dashboard depends on them.")
		} else if parsed, valid := yamlBool(value, false); valid {
			profile.includePresubmits = parsed
		} else {
			add("Pages presubmits", DoctorFail, "include-presubmits is not a boolean", "Set jobs.deploy.with.include-presubmits to true or false.")
		}
	}
	workflowRoot := filepath.Dir(filepath.Dir(filepath.Dir(workflowPath)))
	configuredProjectDir := "."
	if value, ok := deploy.With["project_dir"]; ok {
		configuredProjectDir = strings.TrimSpace(fmt.Sprint(value))
	}
	if strings.Contains(configuredProjectDir, "${{") {
		expectedProjectDir, err := filepath.Rel(workflowRoot, filepath.Clean(projectDir))
		if err != nil {
			expectedProjectDir = "the selected consumer directory relative to the repository root"
		}
		add("Pages project_dir", DoctorWarn, "jobs.deploy.with.project_dir is dynamic and cannot be resolved offline", "Confirm the expression resolves to "+filepath.ToSlash(expectedProjectDir)+".")
	} else {
		resolvedProjectDir := filepath.Clean(filepath.Join(workflowRoot, configuredProjectDir))
		if resolvedProjectDir != filepath.Clean(projectDir) {
			add("Pages project_dir", DoctorFail, "workflow resolves project_dir to "+resolvedProjectDir+", not "+filepath.Clean(projectDir), "Set jobs.deploy.with.project_dir to the consumer directory relative to the repository root.")
		}
	}
	if dynamicExpression(deploy.With["skip-fetch"]) || dynamicExpression(deploy.With["ai"]) {
		add("Pages AI", DoctorWarn, "skip-fetch or ai is dynamic and provider requirements cannot be resolved offline", "Confirm every runtime branch has the provider mappings it uses.")
		return
	}
	skipFetch, valid := yamlBool(deploy.With["skip-fetch"], false)
	if !valid {
		add("Pages AI", DoctorFail, "skip-fetch is not a boolean", "Set jobs.deploy.with.skip-fetch to true or false.")
		return
	}
	if skipFetch {
		// No fetch runs, so triage never reads GitHub and the token is unused.
		profile.readToken = readTokenUnknown
		add("Pages AI", DoctorPass, "skip-fetch is enabled, so provider settings are unused", "")
		return
	}
	aiEnabled, valid := yamlBool(deploy.With["ai"], true)
	if !valid {
		add("Pages AI", DoctorFail, "ai is not a boolean", "Set jobs.deploy.with.ai to true or false.")
		return
	}
	if !aiEnabled {
		add("Pages AI", DoctorPass, "deployed AI analysis is disabled", "")
		return
	}
	var missing []string
	externalValues := []string{"AI_TOKEN"}
	if cfg.AI == nil || strings.TrimSpace(cfg.AI.Endpoint) == "" {
		externalValues = append(externalValues, "AI_ENDPOINT")
		if !githubExpression(deploy.With["ai-endpoint"], "vars", "AI_ENDPOINT") {
			missing = append(missing, "ai-endpoint")
		}
	}
	if cfg.AI == nil || strings.TrimSpace(cfg.AI.Model) == "" {
		externalValues = append(externalValues, "AI_MODEL")
		if !githubExpression(deploy.With["ai-model"], "vars", "AI_MODEL") {
			missing = append(missing, "ai-model")
		}
	}
	if cfg.AI == nil || strings.TrimSpace(cfg.AI.API) == "" {
		if value, ok := deploy.With["ai-api"]; ok {
			externalValues = append(externalValues, "AI_API")
			if !githubExpression(value, "vars", "AI_API") {
				missing = append(missing, "ai-api")
			}
		}
	}
	if value, ok := deploy.With["ai-reasoning-effort"]; ok {
		if !githubExpression(value, "vars", project.AIReasoningEffortEnv) {
			missing = append(missing, "ai-reasoning-effort")
		}
	}
	if !githubExpression(deploy.Secrets["AI_TOKEN"], "secrets", "AI_TOKEN") {
		missing = append(missing, "secrets.AI_TOKEN")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		add("Pages AI", DoctorFail, "deploy job mappings are missing or incorrect: "+strings.Join(missing, ", "), "Regenerate the Pages workflow or repair jobs.deploy.with and jobs.deploy.secrets.")
		return
	}
	add("Pages AI", DoctorPass, "deploy job resolves provider coordinates and token settings", "")
	sort.Strings(externalValues)
	add("Pages AI values", DoctorWarn, "offline doctor cannot read GitHub repository variable or secret values", "Confirm "+strings.Join(externalValues, ", ")+" are set in the dashboard repository.")
	return profile
}

func dynamicExpression(value any) bool {
	return strings.Contains(fmt.Sprint(value), "${{")
}

func reusableDeployReference(value string) bool {
	workflow, ref, ok := strings.Cut(strings.TrimSpace(value), "@")
	if !ok || strings.TrimSpace(ref) == "" {
		return false
	}
	parts := strings.Split(workflow, "/")
	if len(parts) != 5 || parts[0] != "willie-yao" || parts[2] != ".github" || parts[3] != "workflows" || parts[4] != "reusable-deploy.yml" {
		return false
	}
	return parts[1] == "aster" || parts[1] == "prow-ai-dashboard"
}

func githubExpression(value any, scope, name string) bool {
	raw := strings.TrimSpace(fmt.Sprint(value))
	if !strings.HasPrefix(raw, "${{") || !strings.HasSuffix(raw, "}}") {
		return false
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "${{"), "}}"))
	return body == scope+"."+name
}

type doctorLabelSelector struct {
	MatchLabels      map[string]string `yaml:"matchLabels"`
	MatchExpressions []struct {
		Key      string   `yaml:"key"`
		Operator string   `yaml:"operator"`
		Values   []string `yaml:"values"`
	} `yaml:"matchExpressions"`
}

type doctorNetworkPolicyPeer struct {
	PodSelector       *doctorLabelSelector `yaml:"podSelector"`
	NamespaceSelector *doctorLabelSelector `yaml:"namespaceSelector"`
	IPBlock           *struct {
		CIDR string `yaml:"cidr"`
	} `yaml:"ipBlock"`
}

type doctorNetworkPolicyIngressRule struct {
	From []doctorNetworkPolicyPeer `yaml:"from"`
}

type doctorNetworkPolicyValues struct {
	Enabled bool                             `yaml:"enabled"`
	Ingress []doctorNetworkPolicyIngressRule `yaml:"ingress"`
	From    []doctorNetworkPolicyPeer        `yaml:"from"`
}

type doctorKubernetesValues struct {
	Persistence struct {
		Enabled       *bool  `yaml:"enabled"`
		ExistingClaim string `yaml:"existingClaim"`
		StorageClass  string `yaml:"storageClass"`
		AccessMode    string `yaml:"accessMode"`
	} `yaml:"persistence"`
	Fetcher struct {
		IncludePresubmits bool             `yaml:"includePresubmits"`
		ExtraEnv          []doctorExtraEnv `yaml:"extraEnv"`
	} `yaml:"fetcher"`
	AI struct {
		Enabled                   *bool  `yaml:"enabled"`
		API                       string `yaml:"api"`
		Endpoint                  string `yaml:"endpoint"`
		Model                     string `yaml:"model"`
		Token                     string `yaml:"token"`
		ExistingSecret            string `yaml:"existingSecret"`
		GitHubReadToken           string `yaml:"githubReadToken"`
		GitHubReadTokenSecretName string `yaml:"githubReadTokenSecretName"`
	} `yaml:"ai"`
	Server struct {
		Actions struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"actions"`
		ExtraEnv []doctorExtraEnv `yaml:"extraEnv"`
		Chat     struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"chat"`
		RemediationInvestigation struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"remediationInvestigation"`
		PullRequestEscalation struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"pullRequestEscalation"`
		Service struct {
			Type                     string   `yaml:"type"`
			LoadBalancerSourceRanges []string `yaml:"loadBalancerSourceRanges"`
			PublicOriginAcknowledged bool     `yaml:"publicOriginAcknowledged"`
			Internal                 struct {
				Enabled     bool           `yaml:"enabled"`
				Annotations map[string]any `yaml:"annotations"`
			} `yaml:"internal"`
		} `yaml:"service"`
	} `yaml:"server"`
	NetworkPolicy doctorNetworkPolicyValues `yaml:"networkPolicy"`
}

// doctorExtraEnv is one fetcher.extraEnv entry, parsed far enough to tell
// whether it actually supplies a value.
type doctorExtraEnv struct {
	Name      string `yaml:"name"`
	Value     string `yaml:"value"`
	ValueFrom *struct {
		SecretKeyRef *struct {
			Optional *bool `yaml:"optional"`
		} `yaml:"secretKeyRef"`
	} `yaml:"valueFrom"`
}

// envTokenState is what deploy/values.yaml guarantees about the value one
// environment variable will hold.
type envTokenState int

const (
	envTokenMissing envTokenState = iota
	envTokenOptional
	envTokenPresent
)

// state reports what the entry supplies. A name with neither a value nor a
// source injects an empty string, which overrides anything rendered earlier.
func (e doctorExtraEnv) state() envTokenState {
	if strings.TrimSpace(e.Value) != "" {
		return envTokenPresent
	}
	if ref := e.ValueFrom; ref != nil && ref.SecretKeyRef != nil {
		if ref.SecretKeyRef.Optional != nil && *ref.SecretKeyRef.Optional {
			return envTokenOptional
		}
		return envTokenPresent
	}
	return envTokenMissing
}

// fold applies this entry over what an earlier duplicate of the same name left.
// Kubelet skips an optional Secret key that does not exist rather than clearing
// the variable, so an optional entry cannot take away a value already set.
func (e doctorExtraEnv) fold(prior envTokenState) envTokenState {
	if state := e.state(); state != envTokenOptional || prior != envTokenPresent {
		return state
	}
	return envTokenPresent
}

// chartReadTokenState is what the chart's own GITHUB_READ_TOKEN block supplies,
// before any fetcher.extraEnv override.
func chartReadTokenState(values doctorKubernetesValues) envTokenState {
	if !placeholder(values.AI.GitHubReadToken) || !placeholder(values.AI.GitHubReadTokenSecretName) {
		return envTokenPresent
	}
	// ai.existingSecret only reaches the fetcher when AI is on, and the chart
	// mounts that key with optional: true.
	if values.AI.Enabled != nil && *values.AI.Enabled && !placeholder(values.AI.ExistingSecret) {
		return envTokenOptional
	}
	return envTokenMissing
}

// kubernetesReadTokenSource resolves what the fetcher will read, mirroring both
// the chart's rendering conditions and the engine's env preference. Each
// variable is resolved separately because GITHUB_TOKEN is only a fallback, not
// an override of GITHUB_READ_TOKEN.
func kubernetesReadTokenSource(values doctorKubernetesValues) doctorReadTokenSource {
	chart := chartReadTokenState(values)
	read, fallback := chart, envTokenMissing
	// The chart appends fetcher.extraEnv after its own entries and the last
	// duplicate wins, so a later entry decides the final value.
	for _, env := range values.Fetcher.ExtraEnv {
		switch strings.TrimSpace(env.Name) {
		case "GITHUB_READ_TOKEN":
			read = env.fold(read)
		case "GITHUB_TOKEN":
			fallback = env.fold(fallback)
		}
	}
	// githubReadToken prefers GITHUB_READ_TOKEN and falls back to GITHUB_TOKEN,
	// including when an optional Secret key left the preferred one unset.
	switch {
	case read == envTokenPresent || fallback == envTokenPresent:
		return readTokenConfigured
	case read == envTokenOptional || fallback == envTokenOptional:
		return readTokenOptionalSecret
	case chart != envTokenMissing:
		return readTokenShadowed
	default:
		return readTokenAbsent
	}
}

func checkKubernetes(report *DoctorReport, valuesYAML []byte, cfg *project.Config) (profile doctorProfile) {
	add := func(name string, status DoctorStatus, detail, action string) {
		report.Checks = append(report.Checks, DoctorCheck{Name: name, Status: status, Detail: detail, Action: action})
	}
	var values doctorKubernetesValues
	if err := yaml.Unmarshal(valuesYAML, &values); err != nil {
		add("Kubernetes values", DoctorFail, err.Error(), "Fix deploy/values.yaml so it is valid YAML.")
		return
	}
	profile.includePresubmits = values.Fetcher.IncludePresubmits
	profile.readToken = kubernetesReadTokenSource(values)
	if !placeholder(values.Persistence.ExistingClaim) {
		add("Kubernetes storage", DoctorPass, "persistence.existingClaim is configured", "")
	} else if values.Persistence.Enabled != nil && !*values.Persistence.Enabled {
		add("Kubernetes storage", DoctorFail, "persistence is disabled without an existing claim", "Set persistence.existingClaim or enable persistence with a ReadWriteMany storage strategy.")
	} else if placeholder(values.Persistence.StorageClass) {
		add("Kubernetes storage", DoctorFail, "neither persistence.existingClaim nor persistence.storageClass is configured", "Set an existing ReadWriteMany claim or a ReadWriteMany-capable storage class.")
	} else if mode := strings.TrimSpace(values.Persistence.AccessMode); mode != "" && mode != "ReadWriteMany" {
		add("Kubernetes storage", DoctorFail, "persistence.accessMode is "+mode+", not ReadWriteMany", "Set persistence.accessMode to ReadWriteMany or use an existing ReadWriteMany claim.")
	} else {
		add("Kubernetes storage", DoctorPass, "dynamic ReadWriteMany storage is configured", "")
	}
	checkKubernetesOrigin(add, values)
	checkKubernetesPullRequestEscalation(add, values, cfg)
	aiEnabled := values.AI.Enabled != nil && *values.AI.Enabled
	if !aiEnabled {
		add("Kubernetes AI", DoctorPass, "deployed AI analysis is disabled", "")
		return
	}
	api := values.AI.API
	endpoint := values.AI.Endpoint
	model := values.AI.Model
	if cfg.AI != nil {
		if strings.TrimSpace(cfg.AI.API) != "" {
			api = cfg.AI.API
		}
		if strings.TrimSpace(cfg.AI.Endpoint) != "" {
			endpoint = cfg.AI.Endpoint
		}
		if strings.TrimSpace(cfg.AI.Model) != "" {
			model = cfg.AI.Model
		}
	}
	if err := project.ValidateAIAPI(api); err != nil {
		add("Kubernetes AI", DoctorFail, err.Error(), "Set ai.api to chat_completions or responses in project.yaml or deploy/values.yaml.")
		return
	}
	var missing []string
	if placeholder(endpoint) {
		missing = append(missing, "ai.endpoint")
	}
	if placeholder(model) {
		missing = append(missing, "ai.model")
	}
	if len(missing) > 0 {
		add("Kubernetes AI", DoctorFail, "required settings are missing or placeholders: "+strings.Join(missing, ", "), "Set the model endpoint and model id before installing the chart.")
	} else {
		add("Kubernetes AI", DoctorPass, "API, endpoint, and model are configured", "")
	}
	if placeholder(values.AI.Token) && placeholder(values.AI.ExistingSecret) {
		add("Kubernetes AI credential", DoctorWarn, "no token or existing Secret is declared in deploy/values.yaml", "Configure ai.existingSecret and have the organization Secret manager provision the required key.")
	}
	if _, trimmed := textutil.TrimCredential(values.AI.Token); trimmed {
		add("Kubernetes AI credential", DoctorFail, "ai.token has leading or trailing whitespace", "Remove the surrounding whitespace. A credential with a stray newline is rejected by the endpoint as invalid, far from this file.")
	}
	return profile
}

// serverReadTokenState is what the server will hold for escalation's GitHub
// reads. It mirrors the chart's own GITHUB_READ_TOKEN block, the BOT_TOKEN that
// enabled actions always render, and githubReadTokenFromEnv's preference order.
func serverReadTokenState(values doctorKubernetesValues) envTokenState {
	read, bot, github := chartReadTokenState(values), envTokenMissing, envTokenMissing
	// The chart refuses to install enabled actions without a bot token, and the
	// server mounts that key without optional, so the pod cannot start without it.
	if values.Server.Actions.Enabled {
		bot = envTokenPresent
	}
	// The chart appends server.extraEnv after its own entries and the last
	// duplicate wins, so a later entry decides the final value. Each variable is
	// resolved separately because the engine takes the first non-empty one.
	for _, env := range values.Server.ExtraEnv {
		switch strings.TrimSpace(env.Name) {
		case "GITHUB_READ_TOKEN":
			read = env.fold(read)
		case "BOT_TOKEN":
			bot = env.fold(bot)
		case "GITHUB_TOKEN":
			github = env.fold(github)
		}
	}
	switch {
	case read == envTokenPresent || bot == envTokenPresent || github == envTokenPresent:
		return envTokenPresent
	case read == envTokenOptional || bot == envTokenOptional || github == envTokenOptional:
		return envTokenOptional
	default:
		return envTokenMissing
	}
}

// checkKubernetesPullRequestEscalation reports the preconditions the chart
// cannot see: escalation needs a model and the triage view it escalates from.
func checkKubernetesPullRequestEscalation(add func(string, DoctorStatus, string, string), values doctorKubernetesValues, cfg *project.Config) {
	const name = "Kubernetes pull request escalation"
	if !values.Server.PullRequestEscalation.Enabled {
		add(name, DoctorPass, "on-demand pull request escalation is disabled", "")
		return
	}
	if values.AI.Enabled == nil || !*values.AI.Enabled {
		add(name, DoctorFail, "server.pullRequestEscalation.enabled is set while ai.enabled is not", "Set ai.enabled with the provider endpoint and model, or disable server.pullRequestEscalation.enabled. The chart refuses to render this combination.")
		return
	}
	if cfg.PullRequests == nil || !cfg.PullRequests.Enabled {
		add(name, DoctorFail, "server.pullRequestEscalation.enabled is set while pull_requests.enabled is not", "Enable pull_requests in project.yaml. The server refuses to start otherwise, because escalation escalates a triaged pull request failure.")
		return
	}
	switch serverReadTokenState(values) {
	case envTokenMissing:
		add(name, DoctorWarn, "no GitHub read token reaches the server", "Set ai.githubReadTokenSecretName so escalation reads changed files authenticated rather than at the anonymous 60 requests per hour limit.")
	case envTokenOptional:
		add(name, DoctorWarn, "the server's GitHub read token comes from an optional Secret key that may not exist", "Set ai.githubReadTokenSecretName, or confirm that ai.existingSecret carries ai.githubReadTokenSecretKey. A missing optional key silently falls back to anonymous reads.")
	default:
		add(name, DoctorPass, "escalation has a model, pull request triage, and a GitHub read token", "")
	}
}

func checkKubernetesOrigin(add func(string, DoctorStatus, string, string), values doctorKubernetesValues) {
	if !values.Server.Actions.Enabled && !values.Server.Chat.Enabled &&
		!values.Server.RemediationInvestigation.Enabled && !values.Server.PullRequestEscalation.Enabled {
		add("Kubernetes origin security", DoctorPass, "authenticated server features are disabled", "")
		return
	}
	serviceType := strings.TrimSpace(values.Server.Service.Type)
	if serviceType == "" {
		serviceType = "ClusterIP"
	}
	switch serviceType {
	case "ClusterIP":
		if doctorNetworkPolicyRestricted(values.NetworkPolicy) {
			add("Kubernetes origin security", DoctorPass, "authenticated server features use a ClusterIP Service with NetworkPolicy", "")
		} else {
			add("Kubernetes origin security", DoctorWarn, "authenticated server features use a ClusterIP Service without a restrictive NetworkPolicy", "Enable NetworkPolicy and allow only the expected ingress controller or authentication proxy path.")
		}
	case "LoadBalancer":
		if values.Server.Service.Internal.Enabled && len(values.Server.Service.Internal.Annotations) == 0 {
			add("Kubernetes origin security", DoctorWarn, "internal LoadBalancer is enabled without provider annotations", "Set server.service.internal.annotations for the cloud provider and verify that the resulting address is private.")
			return
		}
		restricted := values.Server.Service.Internal.Enabled || doctorRestrictedSourceRanges(values.Server.Service.LoadBalancerSourceRanges)
		if !restricted {
			if values.Server.Service.PublicOriginAcknowledged {
				add("Kubernetes origin security", DoctorWarn, "authenticated server features use an acknowledged public LoadBalancer", "Verify direct origin reachability at runtime and restrict it with source ranges, a private origin, and NetworkPolicy where possible.")
			} else {
				add("Kubernetes origin security", DoctorWarn, "authenticated server features use a public LoadBalancer without source ranges, an explicit internal origin, or acknowledgement", "Prefer ClusterIP, configure an internal LoadBalancer or loadBalancerSourceRanges, and enable NetworkPolicy. Use publicOriginAcknowledged only for an intentional last-resort public origin.")
			}
			return
		}
		if !doctorNetworkPolicyRestricted(values.NetworkPolicy) {
			add("Kubernetes origin security", DoctorWarn, "the LoadBalancer origin is restricted but NetworkPolicy does not restrict ingress", "Enable NetworkPolicy with ingress rules for the expected ingress or proxy path.")
			return
		}
		add("Kubernetes origin security", DoctorPass, "authenticated server features use an origin-restricted LoadBalancer with NetworkPolicy", "")
	default:
		add("Kubernetes origin security", DoctorWarn, "authenticated server features use Service type "+serviceType, "Prefer ClusterIP behind an ingress or an explicitly restricted LoadBalancer, then verify runtime reachability.")
	}
}

func doctorRestrictedSourceRanges(ranges []string) bool {
	if len(ranges) != 1 {
		return false
	}
	for _, cidr := range ranges {
		cidr = strings.TrimSpace(cidr)
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil || prefix.Bits() == 0 {
			return false
		}
	}
	return true
}

func doctorNetworkPolicyRestricted(policy doctorNetworkPolicyValues) bool {
	if !policy.Enabled {
		return false
	}
	if policy.Ingress != nil {
		if len(policy.Ingress) == 0 {
			return true
		}
		ipBlocks := 0
		for _, rule := range policy.Ingress {
			if len(rule.From) == 0 {
				return false
			}
			for _, peer := range rule.From {
				if peer.IPBlock != nil {
					ipBlocks++
				}
				if !doctorNetworkPolicyPeerRestricted(peer) {
					return false
				}
			}
		}
		return ipBlocks <= 1
	}
	if len(policy.From) == 0 {
		return false
	}
	ipBlocks := 0
	for _, peer := range policy.From {
		if peer.IPBlock != nil {
			ipBlocks++
		}
		if !doctorNetworkPolicyPeerRestricted(peer) {
			return false
		}
	}
	return ipBlocks <= 1
}

func doctorNetworkPolicyPeerRestricted(peer doctorNetworkPolicyPeer) bool {
	if peer.IPBlock != nil {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(peer.IPBlock.CIDR))
		return err == nil && prefix.Bits() > 0
	}
	if peer.NamespaceSelector != nil {
		return doctorLabelSelectorRestricted(peer.NamespaceSelector) || doctorLabelSelectorRestricted(peer.PodSelector)
	}
	return doctorLabelSelectorRestricted(peer.PodSelector)
}

func doctorLabelSelectorRestricted(selector *doctorLabelSelector) bool {
	if selector == nil {
		return false
	}
	for key := range selector.MatchLabels {
		if strings.TrimSpace(key) != "" {
			return true
		}
	}
	for _, expression := range selector.MatchExpressions {
		if strings.TrimSpace(expression.Key) == "" {
			continue
		}
		switch expression.Operator {
		case "In":
			if len(expression.Values) > 0 {
				return true
			}
		}
	}
	return false
}

func placeholder(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || strings.Contains(value, "<") || strings.Contains(strings.ToLower(value), "your-")
}

// WriteDoctorReport prints every check and returns output failures.
func WriteDoctorReport(out io.Writer, report DoctorReport) error {
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(out, "[%s] %s: %s\n", check.Status, safeTerminal(check.Name), safeTerminal(check.Detail)); err != nil {
			return err
		}
		if check.Action != "" {
			if _, err := fmt.Fprintf(out, "  next: %s\n", safeTerminal(check.Action)); err != nil {
				return err
			}
		}
	}
	return nil
}
