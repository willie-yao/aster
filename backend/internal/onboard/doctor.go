package onboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
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
	switch {
	case pagesExists && k8sExists:
		add("deployment", DoctorFail, "both Pages and Kubernetes deployment files are present", "Keep one first-run deployment profile and remove the unintended scaffold files.")
	case pagesExists:
		add("deployment", DoctorPass, "GitHub Pages profile detected", "")
		checkPages(&report, pagesPath, dir, pages, cfg)
	case k8sExists:
		add("deployment", DoctorPass, "Kubernetes with Helm profile detected", "")
		checkKubernetes(&report, k8s, cfg)
	default:
		add("deployment", DoctorFail, "no supported deployment scaffold was found", "Restore .github/workflows/deploy.yml or deploy/values.yaml.")
	}

	discoveryCtx, cancel := context.WithTimeout(ctx, onboardingDiscoveryTimeout)
	jobs, err := deps.sweeper.Discover(discoveryCtx, cfg, cfg.Source.IncludePresubmits)
	cancel()
	if err != nil {
		action := "Verify the TestGrid dashboard or artifact bucket, GitHub access, and network connectivity, then rerun doctor."
		add("Prow discovery", DoctorFail, err.Error(), action)
	} else if len(jobs) == 0 {
		add("Prow discovery", DoctorFail, "the real discovery sweep found zero jobs", "Correct testgrid.dashboard or discovery/storage settings until at least one job is found.")
	} else {
		add("Prow discovery", DoctorPass, fmt.Sprintf("the real discovery sweep found %d job(s)", len(jobs)), "")
	}
	return report
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

func yamlBool(value any, defaultValue bool) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		if strings.EqualFold(strings.TrimSpace(typed), "true") {
			return true
		}
		if strings.EqualFold(strings.TrimSpace(typed), "false") {
			return false
		}
	}
	return defaultValue
}

func checkPages(report *DoctorReport, workflowPath, projectDir string, workflowYAML []byte, cfg *project.Config) {
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
	if !strings.Contains(deploy.Uses, "/.github/workflows/reusable-deploy.yml@") {
		add("Pages workflow", DoctorFail, "jobs.deploy.uses does not target the dashboard reusable-deploy workflow", "Restore the generated uses target for prow-ai-dashboard/.github/workflows/reusable-deploy.yml.")
		return
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
	if yamlBool(deploy.With["skip-fetch"], false) {
		add("Pages AI", DoctorPass, "skip-fetch is enabled, so provider settings are unused", "")
		return
	}
	aiEnabled := yamlBool(deploy.With["ai"], true)
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
	if value, ok := deploy.With["ai-api"]; ok {
		externalValues = append(externalValues, "AI_API")
		if !githubExpression(value, "vars", "AI_API") {
			missing = append(missing, "ai-api")
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
}

func githubExpression(value any, scope, name string) bool {
	raw := strings.TrimSpace(fmt.Sprint(value))
	if !strings.HasPrefix(raw, "${{") || !strings.HasSuffix(raw, "}}") {
		return false
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "${{"), "}}"))
	body = strings.Join(strings.Fields(body), "")
	return body == scope+"."+name
}

type doctorKubernetesValues struct {
	Persistence struct {
		Enabled       *bool  `yaml:"enabled"`
		ExistingClaim string `yaml:"existingClaim"`
		StorageClass  string `yaml:"storageClass"`
		AccessMode    string `yaml:"accessMode"`
	} `yaml:"persistence"`
	AI struct {
		Enabled        *bool  `yaml:"enabled"`
		API            string `yaml:"api"`
		Endpoint       string `yaml:"endpoint"`
		Model          string `yaml:"model"`
		Token          string `yaml:"token"`
		ExistingSecret string `yaml:"existingSecret"`
	} `yaml:"ai"`
}

func checkKubernetes(report *DoctorReport, valuesYAML []byte, cfg *project.Config) {
	add := func(name string, status DoctorStatus, detail, action string) {
		report.Checks = append(report.Checks, DoctorCheck{Name: name, Status: status, Detail: detail, Action: action})
	}
	var values doctorKubernetesValues
	if err := yaml.Unmarshal(valuesYAML, &values); err != nil {
		add("Kubernetes values", DoctorFail, err.Error(), "Fix deploy/values.yaml so it is valid YAML.")
		return
	}
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
	if strings.TrimSpace(values.AI.Token) == "" && strings.TrimSpace(values.AI.ExistingSecret) == "" {
		add("Kubernetes AI credential", DoctorWarn, "no token or existing Secret is declared in deploy/values.yaml", "Supply --set ai.token at install time or configure ai.existingSecret.")
	}
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
