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

func runDoctor(ctx context.Context, opts DoctorOptions, deps doctorDependencies) DoctorReport {
	dir := strings.TrimSpace(opts.ProjectDir)
	if dir == "" {
		dir = "."
	}
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
	if err != nil || strings.TrimSpace(string(prompt)) == "" {
		add("prompts/system.md", DoctorFail, "the required project prompt is missing or empty", "Create a non-empty prompts/system.md and review its project-specific claims.")
	} else {
		add("prompts/system.md", DoctorPass, "required project prompt is present", "")
	}

	pagesPath := filepath.Join(dir, ".github", "workflows", "deploy.yml")
	k8sPath := filepath.Join(dir, "deploy", "values.yaml")
	pages, pagesErr := deps.files.ReadFile(pagesPath)
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
		checkPages(&report, pages)
	case k8sExists:
		add("deployment", DoctorPass, "Kubernetes with Helm profile detected", "")
		checkKubernetes(&report, k8s)
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

func checkPages(report *DoctorReport, workflowYAML []byte) {
	add := func(name string, status DoctorStatus, detail, action string) {
		report.Checks = append(report.Checks, DoctorCheck{Name: name, Status: status, Detail: detail, Action: action})
	}
	type workflowJob struct {
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
	aiEnabled := true
	if value, ok := deploy.With["ai"]; ok {
		switch typed := value.(type) {
		case bool:
			aiEnabled = typed
		case string:
			aiEnabled = !strings.EqualFold(strings.TrimSpace(typed), "false")
		}
	}
	if !aiEnabled {
		add("Pages AI", DoctorPass, "deployed AI analysis is disabled", "")
		return
	}
	requiredWith := map[string]string{
		"ai-api":      "vars.AI_API",
		"ai-endpoint": "vars.AI_ENDPOINT",
		"ai-model":    "vars.AI_MODEL",
	}
	var missing []string
	for key, marker := range requiredWith {
		if !strings.Contains(fmt.Sprint(deploy.With[key]), marker) {
			missing = append(missing, key)
		}
	}
	if !strings.Contains(fmt.Sprint(deploy.Secrets["AI_TOKEN"]), "secrets.AI_TOKEN") {
		missing = append(missing, "secrets.AI_TOKEN")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		add("Pages AI", DoctorFail, "deploy job mappings are missing or incorrect: "+strings.Join(missing, ", "), "Regenerate the Pages workflow or repair jobs.deploy.with and jobs.deploy.secrets.")
		return
	}
	add("Pages AI", DoctorPass, "deploy job maps API, endpoint, model, and token settings", "")
	add("Pages AI values", DoctorWarn, "offline doctor cannot read GitHub repository variable or secret values", "Confirm AI_API, AI_ENDPOINT, AI_MODEL, and AI_TOKEN are set in the dashboard repository.")
}

type doctorKubernetesValues struct {
	Persistence struct {
		ExistingClaim string `yaml:"existingClaim"`
		StorageClass  string `yaml:"storageClass"`
		AccessMode    string `yaml:"accessMode"`
	} `yaml:"persistence"`
	AI struct {
		Enabled  *bool  `yaml:"enabled"`
		API      string `yaml:"api"`
		Endpoint string `yaml:"endpoint"`
		Model    string `yaml:"model"`
	} `yaml:"ai"`
}

func checkKubernetes(report *DoctorReport, valuesYAML []byte) {
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
	if err := project.ValidateAIAPI(values.AI.API); err != nil {
		add("Kubernetes AI", DoctorFail, err.Error(), "Set ai.api to chat_completions or responses.")
		return
	}
	var missing []string
	if placeholder(values.AI.Endpoint) {
		missing = append(missing, "ai.endpoint")
	}
	if placeholder(values.AI.Model) {
		missing = append(missing, "ai.model")
	}
	if len(missing) > 0 {
		add("Kubernetes AI", DoctorFail, "required settings are missing or placeholders: "+strings.Join(missing, ", "), "Set the model endpoint and model id before installing the chart.")
	} else {
		add("Kubernetes AI", DoctorPass, "API, endpoint, and model are configured", "")
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
