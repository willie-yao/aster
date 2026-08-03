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
	includePresubmits := cfg.Source.IncludePresubmits
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
		profilePresubmits := checkPages(&report, pagesPath, dir, pages, cfg)
		includePresubmits = includePresubmits || profilePresubmits
	case k8sExists:
		add("deployment", DoctorPass, "Kubernetes with Helm profile detected", "")
		profilePresubmits := checkKubernetes(&report, k8s, cfg)
		includePresubmits = includePresubmits || profilePresubmits
	default:
		add("deployment", DoctorFail, "no supported deployment scaffold was found", "Restore .github/workflows/deploy.yml or deploy/values.yaml.")
	}

	discoveryCtx, cancel := context.WithTimeout(ctx, onboardingDiscoveryTimeout)
	jobs, err := deps.sweeper.Discover(discoveryCtx, cfg, includePresubmits)
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

func checkPages(report *DoctorReport, workflowPath, projectDir string, workflowYAML []byte, cfg *project.Config) (includePresubmits bool) {
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
		add("Pages workflow", DoctorFail, "jobs.deploy.uses does not target the dashboard reusable-deploy workflow", "Restore the generated uses target for prow-ai-dashboard/.github/workflows/reusable-deploy.yml.")
		return
	}
	if value, ok := deploy.With["include-presubmits"]; ok {
		if dynamicExpression(value) {
			add("Pages presubmits", DoctorWarn, "include-presubmits is dynamic and cannot be resolved offline", "Confirm the expression enables presubmits when the dashboard depends on them.")
		} else if parsed, valid := yamlBool(value, false); valid {
			includePresubmits = parsed
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
	return includePresubmits
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
	return len(parts) == 5 && parts[0] == "willie-yao" && parts[1] == "prow-ai-dashboard" &&
		parts[2] == ".github" && parts[3] == "workflows" && parts[4] == "reusable-deploy.yml"
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
		IncludePresubmits bool `yaml:"includePresubmits"`
	} `yaml:"fetcher"`
	AI struct {
		Enabled        *bool  `yaml:"enabled"`
		API            string `yaml:"api"`
		Endpoint       string `yaml:"endpoint"`
		Model          string `yaml:"model"`
		Token          string `yaml:"token"`
		ExistingSecret string `yaml:"existingSecret"`
	} `yaml:"ai"`
	Server struct {
		Actions struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"actions"`
		Chat struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"chat"`
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

func checkKubernetes(report *DoctorReport, valuesYAML []byte, cfg *project.Config) (includePresubmits bool) {
	add := func(name string, status DoctorStatus, detail, action string) {
		report.Checks = append(report.Checks, DoctorCheck{Name: name, Status: status, Detail: detail, Action: action})
	}
	var values doctorKubernetesValues
	if err := yaml.Unmarshal(valuesYAML, &values); err != nil {
		add("Kubernetes values", DoctorFail, err.Error(), "Fix deploy/values.yaml so it is valid YAML.")
		return
	}
	includePresubmits = values.Fetcher.IncludePresubmits
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
		add("Kubernetes AI credential", DoctorWarn, "no token or existing Secret is declared in deploy/values.yaml", "Supply --set ai.token at install time or configure ai.existingSecret.")
	}
	return includePresubmits
}

func checkKubernetesOrigin(add func(string, DoctorStatus, string, string), values doctorKubernetesValues) {
	if !values.Server.Actions.Enabled && !values.Server.Chat.Enabled {
		add("Kubernetes origin security", DoctorPass, "authenticated actions and chat are disabled", "")
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
