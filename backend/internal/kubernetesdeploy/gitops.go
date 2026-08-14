package kubernetesdeploy

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/willie-yao/aster/backend/internal/modelprovider"
	"gopkg.in/yaml.v3"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

const (
	DefaultPlatformChart = "oci://ghcr.io/willie-yao/charts/aster-platform"
	gitOpsValuesKey      = "values.yaml"
)

var (
	gitOpsCredentialPattern = regexp.MustCompile(`(?i)(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-(?:proj-)?[A-Za-z0-9_-]{20,}|xox[baprs]-[A-Za-z0-9-]{20,}|-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----|(?:AI_TOKEN|GITHUB_TOKEN|BOT_TOKEN|CLIENT_SECRET|SESSION_KEY|SMTP_PASSWORD|PASSWORD|PRIVATE_KEY)\s*[:=]\s*[^\s"']{8,})`)
	gitOpsLocalPathPattern  = regexp.MustCompile(`(?i)(?:/Users/[^/\s]+/|/home/[^/\s]+/|[A-Z]:\\Users\\[^\\\s]+\\|(?:^|[[:space:]"'])~?/\.kube/config)`)
	gitOpsSecretLeafKeys    = map[string]bool{
		"token": true, "password": true, "clientsecret": true, "sessionkey": true,
		"bottoken": true, "privatekey": true, "smtppassword": true, "secret": true,
	}
)

// GitOpsOptions selects a provider-free Flux bundle.
type GitOpsOptions struct {
	ProjectDir         string
	ValuesFile         string
	PlatformValuesFile string
	Release            string
	Namespace          string
	ExecutionNamespace string
	Chart              string
	PlatformChart      string
	ChartVersion       string
	OutputDir          string
	DryRun             bool
}

// RenderGitOps validates consumer inputs and writes a deterministic Flux bundle.
func RenderGitOps(opts GitOpsOptions, out io.Writer) error {
	plan, resolved, err := buildGitOpsPlan(opts)
	if err != nil {
		return err
	}
	if out == nil {
		out = io.Discard
	}
	actions, err := planGitOpsWrites(resolved.ProjectDir, resolved.OutputDir, plan)
	if err != nil {
		return err
	}
	if resolved.DryRun {
		fmt.Fprintf(out, "GitOps render plan: %d file actions under %s\n", len(actions), displayProjectPath(resolved.ProjectDir, resolved.OutputDir))
		for _, action := range actions {
			fmt.Fprintf(out, "  %s\n", action)
		}
		return nil
	}
	if err := applyGitOpsWrites(resolved.ProjectDir, resolved.OutputDir, plan, actions); err != nil {
		return err
	}
	fmt.Fprintf(out, "Generated %d Flux GitOps files under %s.\n", len(plan), displayProjectPath(resolved.ProjectDir, resolved.OutputDir))
	for _, action := range actions {
		fmt.Fprintf(out, "  %s\n", action)
	}
	return nil
}

// CheckGitOps regenerates and byte-compares the expected Flux bundle without network or cluster access.
func CheckGitOps(opts GitOpsOptions, out io.Writer) error {
	plan, resolved, err := buildGitOpsPlan(opts)
	if err != nil {
		return err
	}
	if out == nil {
		out = io.Discard
	}
	root := resolved.OutputDir
	var problems []string
	for _, name := range sortedGitOpsPaths(plan) {
		data, err := readSafeGeneratedFile(resolved.ProjectDir, root, name)
		if err != nil {
			if os.IsNotExist(err) {
				problems = append(problems, "missing "+name)
				continue
			}
			return err
		}
		if !bytes.Equal(data, plan[name]) {
			problems = append(problems, "stale "+name)
		}
	}
	actual, err := listGitOpsFiles(resolved.ProjectDir, root)
	if err != nil {
		return err
	}
	for _, name := range actual {
		if _, ok := plan[name]; !ok {
			problems = append(problems, "unexpected "+name)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		for _, problem := range problems {
			fmt.Fprintln(out, problem)
		}
		return fmt.Errorf("GitOps bundle is not current: %d file differences", len(problems))
	}
	fmt.Fprintf(out, "GitOps bundle is current: %d generated files.\n", len(plan))
	return nil
}

type gitOpsResolved struct {
	GitOpsOptions
	FixEnabled bool
}

func buildGitOpsPlan(opts GitOpsOptions) (map[string][]byte, gitOpsResolved, error) {
	resolved, appValues, platformValues, projectYAML, prompt, skillData, err := resolveGitOpsInputs(opts)
	if err != nil {
		return nil, gitOpsResolved{}, err
	}
	appValues["project"] = map[string]any{
		"config":       string(projectYAML),
		"systemPrompt": string(prompt),
		"skills":       skillData,
	}
	applicationValuesYAML, err := marshalYAML(appValues)
	if err != nil {
		return nil, gitOpsResolved{}, fmt.Errorf("render application values: %w", err)
	}
	plan := map[string][]byte{
		"README.md":                         []byte(gitOpsReadme),
		"application/namespace.yaml":        []byte(renderGitOpsNamespace(resolved.Namespace)),
		"application/values-configmap.yaml": []byte(renderGitOpsValuesConfigMap(applicationValuesName(resolved.Release), resolved.Namespace, applicationValuesYAML)),
		"application/helmrelease.yaml":      []byte(renderGitOpsHelmRelease(resolved, false)),
		"application/kustomization.yaml":    []byte(renderKustomization("namespace.yaml", "values-configmap.yaml", "helmrelease.yaml")),
		"sources/application-chart.yaml":    []byte(renderGitOpsOCIRepository(applicationChartSourceName(resolved.Release), resolved.Namespace, resolved.Chart, resolved.ChartVersion)),
		"sources/kustomization.yaml":        []byte(renderKustomization("application-chart.yaml")),
	}
	rootResources := []string{"sources"}
	if resolved.FixEnabled {
		platformValuesYAML, err := marshalYAML(platformValues)
		if err != nil {
			return nil, gitOpsResolved{}, fmt.Errorf("render platform values: %w", err)
		}
		plan["platform/values-configmap.yaml"] = []byte(renderGitOpsValuesConfigMap(platformValuesName(resolved.Release), resolved.Namespace, platformValuesYAML))
		plan["platform/helmrelease.yaml"] = []byte(renderGitOpsHelmRelease(resolved, true))
		plan["platform/kustomization.yaml"] = []byte(renderKustomization("values-configmap.yaml", "helmrelease.yaml"))
		plan["sources/platform-chart.yaml"] = []byte(renderGitOpsOCIRepository(platformChartSourceName(resolved.Release), resolved.Namespace, resolved.PlatformChart, resolved.ChartVersion))
		plan["sources/kustomization.yaml"] = []byte(renderKustomization("application-chart.yaml", "platform-chart.yaml"))
		rootResources = append(rootResources, "platform")
	}
	rootResources = append(rootResources, "application")
	plan["kustomization.yaml"] = []byte(renderKustomization(rootResources...))
	if err := validateGitOpsPlan(plan, resolved); err != nil {
		return nil, gitOpsResolved{}, err
	}
	return plan, resolved, nil
}

func resolveGitOpsInputs(opts GitOpsOptions) (gitOpsResolved, map[string]any, map[string]any, []byte, []byte, map[string]any, error) {
	if strings.TrimSpace(opts.ProjectDir) == "" {
		opts.ProjectDir = "."
	}
	projectDir, err := filepath.Abs(opts.ProjectDir)
	if err != nil {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("resolve project directory: %w", err)
	}
	projectDir, err = filepath.EvalSymlinks(projectDir)
	if err != nil {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("resolve project directory: %w", err)
	}
	opts.ProjectDir = filepath.Clean(projectDir)
	if strings.TrimSpace(opts.ValuesFile) == "" {
		opts.ValuesFile = filepath.Join("deploy", "values.yaml")
	}
	if strings.TrimSpace(opts.PlatformValuesFile) == "" {
		opts.PlatformValuesFile = filepath.Join("deploy", "platform-values.yaml")
	}
	if strings.TrimSpace(opts.OutputDir) == "" {
		opts.OutputDir = "gitops"
	}
	for label, value := range map[string]*string{
		"application values": &opts.ValuesFile,
		"platform values":    &opts.PlatformValuesFile,
		"output":             &opts.OutputDir,
	} {
		path, err := safeProjectPath(opts.ProjectDir, *value, label, label == "output" || label == "platform values")
		if err != nil {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, err
		}
		*value = path
	}
	if strings.TrimSpace(opts.Release) == "" || strings.TrimSpace(opts.Namespace) == "" {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("--release and --namespace are required")
	}
	if len(opts.Release) > 53 {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("release %q is invalid: Helm release names are limited to 53 characters", opts.Release)
	}
	if problems := k8svalidation.IsDNS1123Label(opts.Release); len(problems) > 0 {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("release %q is invalid: %s", opts.Release, strings.Join(problems, ", "))
	}
	if problems := k8svalidation.IsDNS1123Label(opts.Namespace); len(problems) > 0 {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("namespace %q is invalid: %s", opts.Namespace, strings.Join(problems, ", "))
	}
	if opts.ExecutionNamespace != "" {
		if problems := k8svalidation.IsDNS1123Label(opts.ExecutionNamespace); len(problems) > 0 {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("execution namespace %q is invalid: %s", opts.ExecutionNamespace, strings.Join(problems, ", "))
		}
	}
	for label, value := range map[string]string{
		"application values ConfigMap": applicationValuesName(opts.Release),
		"application chart source":     applicationChartSourceName(opts.Release),
		"platform values ConfigMap":    platformValuesName(opts.Release),
		"platform chart source":        platformChartSourceName(opts.Release),
		"platform release":             platformReleaseName(opts.Release),
	} {
		if problems := k8svalidation.IsDNS1123Subdomain(value); len(problems) > 0 {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("generated %s name %q is invalid: %s", label, value, strings.Join(problems, ", "))
		}
	}
	if strings.TrimSpace(opts.Chart) == "" {
		opts.Chart = DefaultChart
	}
	if strings.TrimSpace(opts.PlatformChart) == "" {
		opts.PlatformChart = DefaultPlatformChart
	}
	if err := validateGitOpsVersion(opts.ChartVersion); err != nil {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, err
	}
	if err := validateOCIChart("application", opts.Chart); err != nil {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, err
	}
	if err := validateOCIChart("platform", opts.PlatformChart); err != nil {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, err
	}
	for _, path := range []string{filepath.Join(opts.ProjectDir, "project.yaml"), filepath.Join(opts.ProjectDir, "prompts", "system.md"), opts.ValuesFile} {
		if err := requireSafeRegularFile(opts.ProjectDir, path); err != nil {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, err
		}
	}
	projectDir, valuesFile, skillPaths, err := validateLocalBundle(opts.ProjectDir, opts.ValuesFile)
	if err != nil {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("validate GitOps consumer bundle: %w", err)
	}
	opts.ProjectDir, opts.ValuesFile = projectDir, valuesFile
	projectYAML, err := os.ReadFile(filepath.Join(opts.ProjectDir, "project.yaml"))
	if err != nil {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("read project.yaml: %w", err)
	}
	prompt, err := os.ReadFile(filepath.Join(opts.ProjectDir, "prompts", "system.md"))
	if err != nil {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("read prompts/system.md: %w", err)
	}
	appValuesRaw, err := os.ReadFile(opts.ValuesFile)
	if err != nil {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("read application values: %w", err)
	}
	appValues, err := decodeYAMLMap(appValuesRaw, "application values")
	if err != nil {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, err
	}
	var typedValues kubernetesDoctorValues
	if err := yaml.Unmarshal(appValuesRaw, &typedValues); err != nil {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("parse typed application values: %w", err)
	}
	if inline := inlineCredentialFields(typedValues); len(inline) > 0 {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("inline credential fields are not allowed in GitOps values: %s", strings.Join(inline, ", "))
	}
	if _, exists := appValues["project"]; exists {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("application values must not contain top-level project; Aster generates the canonical project bundle")
	}
	fixEnabled := nestedBoolValue(appValues, "agentSandbox", "fixRuntime", "enabled")
	if fixEnabled != typedValues.AgentSandbox.FixRuntime.Enabled {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("application values contain an ambiguous Agent Sandbox Fix selection")
	}
	if fixEnabled {
		if len(platformReleaseName(opts.Release)) > 53 {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("fix-enabled application release names are limited to 44 characters so the paired platform release remains valid")
		}
		fix := typedValues.AgentSandbox.FixRuntime
		if strings.TrimSpace(fix.Image.Repository) == "" || !sha256DigestPattern.MatchString(strings.TrimSpace(fix.Image.Digest)) {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("agent sandbox fix executor image requires a repository and sha256 digest")
		}
		provider := fix.ModelProvider
		config := modelprovider.Normalize(modelprovider.Config{
			CredentialMode: provider.CredentialMode, API: provider.API, Endpoint: provider.Endpoint, Model: provider.Model,
			Auth: modelprovider.Auth{Type: provider.Auth.Type}, PublicCAPrivateDNS: provider.PublicCAPrivateDNS,
		})
		if err := modelprovider.ValidateDeploymentEndpoint(config); err != nil {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("validate Agent Sandbox provider endpoint: %w", err)
		}
	}
	var platformValues map[string]any
	if fixEnabled {
		if strings.TrimSpace(opts.ExecutionNamespace) == "" {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("--execution-namespace is required when agentSandbox.fixRuntime.enabled is true")
		}
		if err := requireSafeRegularFile(opts.ProjectDir, opts.PlatformValuesFile); err != nil {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, err
		}
		platformRaw, err := os.ReadFile(opts.PlatformValuesFile)
		if err != nil {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("read platform values: %w", err)
		}
		platformValues, err = decodeYAMLMap(platformRaw, "platform values")
		if err != nil {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, err
		}
		if err := validatePlatformPairing(opts, appValues, platformValues); err != nil {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, err
		}
	}
	skillData := map[string]any{}
	for _, path := range skillPaths {
		if err := requireSafeRegularFile(opts.ProjectDir, path); err != nil {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, fmt.Errorf("read consumer skill %s: %w", filepath.Base(path), err)
		}
		skillData[filepath.Base(path)] = string(data)
	}
	for label, data := range map[string][]byte{"project.yaml": projectYAML, "prompts/system.md": prompt, "application values": appValuesRaw} {
		if err := rejectCredentialMaterial(label, data); err != nil {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, err
		}
	}
	for name, raw := range skillData {
		if err := rejectCredentialMaterial("skills/"+name, []byte(raw.(string))); err != nil {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, err
		}
	}
	if fixEnabled {
		platformYAML, _ := marshalYAML(platformValues)
		if err := rejectCredentialMaterial("platform values", platformYAML); err != nil {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, err
		}
	}
	if err := rejectInlineCredentials(appValues, nil); err != nil {
		return gitOpsResolved{}, nil, nil, nil, nil, nil, err
	}
	if fixEnabled {
		if err := rejectInlineCredentials(platformValues, nil); err != nil {
			return gitOpsResolved{}, nil, nil, nil, nil, nil, err
		}
	}
	resolved := gitOpsResolved{GitOpsOptions: opts, FixEnabled: fixEnabled}
	return resolved, appValues, platformValues, projectYAML, prompt, skillData, nil
}

func validatePlatformPairing(opts GitOpsOptions, app, platform map[string]any) error {
	pairs := []struct{ label, got, want string }{
		{"application release", nestedStringValue(platform, "application", "releaseName"), opts.Release},
		{"execution namespace", nestedStringValue(platform, "execution", "namespace"), opts.ExecutionNamespace},
		{"application Fix namespace", nestedStringValue(app, "agentSandbox", "fixRuntime", "namespace"), opts.ExecutionNamespace},
		{"runtime class", nestedStringValue(platform, "execution", "runtimeClassName"), nestedStringValue(app, "agentSandbox", "fixRuntime", "runtimeClassName")},
		{"workload ServiceAccount", nestedStringValue(platform, "execution", "workloadServiceAccountName"), nestedStringValue(app, "agentSandbox", "fixRuntime", "workloadServiceAccount", "name")},
	}
	for _, pair := range pairs {
		if strings.TrimSpace(pair.got) == "" || strings.TrimSpace(pair.want) == "" || pair.got != pair.want {
			return fmt.Errorf("platform %s %q must match application value %q", pair.label, pair.got, pair.want)
		}
	}
	return nil
}

func validateGitOpsVersion(version string) error {
	version = strings.TrimSpace(version)
	lower := strings.ToLower(version)
	if version == "" || !semverTagPattern.MatchString(version) || strings.Contains(lower, "snapshot") || lower == "main" || lower == "latest" {
		return fmt.Errorf("--chart-version must be an exact immutable semantic version, not main, latest, or a snapshot")
	}
	if strings.Contains(version, "+") {
		return fmt.Errorf("--chart-version must also be a valid OCI tag and cannot contain semantic-version build metadata")
	}
	return nil
}

func validateOCIChart(label, value string) error {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "oci://") || strings.ContainsAny(value, "?#") || strings.Contains(value, "@") {
		return fmt.Errorf("%s chart must be a credential-free OCI repository URL", label)
	}
	rest := strings.TrimPrefix(value, "oci://")
	parsed, err := url.Parse("https://" + rest)
	if err != nil || parsed.User != nil || parsed.Host == "" || strings.LastIndex(rest, ":") > strings.LastIndex(rest, "/") {
		return fmt.Errorf("%s chart must be a credential-free OCI repository URL without an embedded tag", label)
	}
	return nil
}

func safeProjectPath(projectDir, value, label string, allowMissing bool) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s path is required", label)
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(projectDir, path)
	}
	path = filepath.Clean(path)
	if !pathWithin(projectDir, path) {
		return "", fmt.Errorf("%s path must remain inside the consumer repository", label)
	}
	if err := rejectSymlinkPath(projectDir, path, allowMissing); err != nil {
		return "", fmt.Errorf("unsafe %s path: %w", label, err)
	}
	return path, nil
}

func requireSafeRegularFile(projectDir, path string) error {
	if !pathWithin(projectDir, path) {
		return fmt.Errorf("file path must remain inside the consumer repository")
	}
	if err := rejectSymlinkPath(projectDir, path, false); err != nil {
		return fmt.Errorf("unsafe input path: %w", err)
	}
	return requireRegularFile(path, "GitOps input")
}

func rejectSymlinkPath(root, path string, allowMissing bool) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes project directory")
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if allowMissing && os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path contains symlink %s", displayProjectPath(root, current))
		}
	}
	return nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func decodeYAMLMap(data []byte, label string) (map[string]any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("parse %s: %w", label, err)
	}
	if err := rejectDuplicateYAMLKeys(&document, nil); err != nil {
		return nil, fmt.Errorf("parse %s: %w", label, err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parse %s: multiple YAML documents are not allowed", label)
		}
		return nil, fmt.Errorf("parse %s: %w", label, err)
	}
	out := map[string]any{}
	if len(document.Content) > 0 {
		if err := document.Decode(&out); err != nil {
			return nil, fmt.Errorf("parse %s: expected one YAML mapping: %w", label, err)
		}
	}
	return out, nil
}

func rejectDuplicateYAMLKeys(node *yaml.Node, path []string) error {
	if node.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index].Value
			if seen[key] {
				return fmt.Errorf("duplicate key %s", strings.Join(append(path, key), "."))
			}
			seen[key] = true
			if err := rejectDuplicateYAMLKeys(node.Content[index+1], append(path, key)); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range node.Content {
		if err := rejectDuplicateYAMLKeys(child, path); err != nil {
			return err
		}
	}
	return nil
}

func marshalYAML(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func rejectCredentialMaterial(label string, data []byte) error {
	if gitOpsCredentialPattern.Match(data) {
		return fmt.Errorf("%s contains credential-like material; generated GitOps ConfigMaps may contain references but never Secret values", label)
	}
	if gitOpsLocalPathPattern.Match(data) {
		return fmt.Errorf("%s contains a local workstation or kubeconfig path", label)
	}
	for _, field := range strings.Fields(string(data)) {
		if !strings.Contains(field, "://") {
			continue
		}
		u, err := url.Parse(strings.Trim(field, `"'(),`))
		if err != nil {
			continue
		}
		if u.User != nil {
			return fmt.Errorf("%s contains URL userinfo", label)
		}
		for key, values := range u.Query() {
			lower := strings.ToLower(key)
			if len(values) > 0 && values[0] != "" && (strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "key")) {
				return fmt.Errorf("%s contains a credential-bearing URL query", label)
			}
		}
	}
	return nil
}

func rejectInlineCredentials(value any, path []string) error {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			childPath := append(append([]string(nil), path...), key)
			lower := strings.ToLower(strings.ReplaceAll(key, "_", ""))
			if gitOpsSecretLeafKeys[lower] && !allowedCredentialReference(childPath) && nonemptyScalar(typed[key]) {
				return fmt.Errorf("inline credential field %s is not allowed in GitOps values", strings.Join(childPath, "."))
			}
			if err := rejectInlineCredentials(typed[key], childPath); err != nil {
				return err
			}
		}
	case []any:
		for index, item := range typed {
			if err := rejectInlineCredentials(item, append(path, fmt.Sprintf("[%d]", index))); err != nil {
				return err
			}
		}
	case string:
		if len(path) > 0 {
			leaf := strings.ToLower(path[len(path)-1])
			if (leaf == "tag" || leaf == "repository" || leaf == "image") && !immutableImageIdentity(leaf, typed) {
				return fmt.Errorf("mutable image identity at %s is not allowed", strings.Join(path, "."))
			}
		}
	}
	return nil
}

func immutableImageIdentity(field, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	identity := value
	if field != "tag" {
		if at := strings.LastIndex(identity, "@"); at >= 0 {
			return sha256DigestPattern.MatchString(identity[at+1:])
		}
		colon, slash := strings.LastIndex(identity, ":"), strings.LastIndex(identity, "/")
		if colon <= slash {
			return true
		}
		identity = identity[colon+1:]
	}
	return shaTagPattern.MatchString(identity) || semverTagPattern.MatchString(identity) || sha256DigestPattern.MatchString(identity)
}

func allowedCredentialReference(path []string) bool {
	if len(path) == 0 {
		return false
	}
	leaf := strings.ToLower(path[len(path)-1])
	return strings.Contains(leaf, "existingsecret") || strings.HasSuffix(leaf, "secretname") || strings.HasSuffix(leaf, "tokenkey") || leaf == "key"
}

func nonemptyScalar(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case nil:
		return false
	default:
		return true
	}
}

func nestedValue(root map[string]any, fields ...string) any {
	var current any = root
	for _, field := range fields {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[field]
	}
	return current
}

func nestedStringValue(root map[string]any, fields ...string) string {
	value, _ := nestedValue(root, fields...).(string)
	return value
}

func nestedBoolValue(root map[string]any, fields ...string) bool {
	value, _ := nestedValue(root, fields...).(bool)
	return value
}

func renderGitOpsNamespace(namespace string) string {
	return fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", namespace)
}

func renderGitOpsValuesConfigMap(name, namespace string, values []byte) string {
	return fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: %s\n  labels:\n    reconcile.fluxcd.io/watch: Enabled\ndata:\n  %s: |\n%s", name, namespace, gitOpsValuesKey, indentYAML(values, 4))
}

func renderGitOpsOCIRepository(name, namespace, chart, version string) string {
	return fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: %s
  namespace: %s
spec:
  interval: 30m
  provider: generic
  url: %s
  ref:
    tag: %q
  layerSelector:
    mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
    operation: copy
`, name, namespace, chart, version)
}

func renderGitOpsHelmRelease(resolved gitOpsResolved, platform bool) string {
	name := resolved.Release
	source := applicationChartSourceName(resolved.Release)
	values := applicationValuesName(resolved.Release)
	dependsOn := ""
	if platform {
		name = platformReleaseName(resolved.Release)
		source = platformChartSourceName(resolved.Release)
		values = platformValuesName(resolved.Release)
	} else if resolved.FixEnabled {
		dependsOn = fmt.Sprintf("  dependsOn:\n    - name: %s\n", platformReleaseName(resolved.Release))
	}
	return fmt.Sprintf(`apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: %s
  namespace: %s
spec:
  interval: 30m
  releaseName: %s
  targetNamespace: %s
  storageNamespace: %s
%s  chartRef:
    kind: OCIRepository
    name: %s
  timeout: 15m
  driftDetection:
    mode: enabled
  install:
    createNamespace: false
    strategy:
      name: RemediateOnFailure
    remediation:
      retries: 3
      remediateLastFailure: true
  upgrade:
    cleanupOnFail: true
    strategy:
      name: RemediateOnFailure
    remediation:
      retries: 3
      strategy: rollback
      remediateLastFailure: true
  valuesFrom:
    - kind: ConfigMap
      name: %s
      valuesKey: %s
`, name, resolved.Namespace, name, resolved.Namespace, resolved.Namespace, dependsOn, source, values, gitOpsValuesKey)
}

func renderKustomization(resources ...string) string {
	var out strings.Builder
	out.WriteString("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n")
	for _, resource := range resources {
		fmt.Fprintf(&out, "  - %s\n", resource)
	}
	return out.String()
}

func indentYAML(data []byte, spaces int) string {
	prefix := strings.Repeat(" ", spaces)
	text := strings.TrimSuffix(string(data), "\n")
	return prefix + strings.ReplaceAll(text, "\n", "\n"+prefix) + "\n"
}

func applicationValuesName(release string) string      { return release + "-application-values" }
func platformValuesName(release string) string         { return release + "-platform-values" }
func applicationChartSourceName(release string) string { return release + "-application-chart" }
func platformChartSourceName(release string) string    { return release + "-platform-chart" }
func platformReleaseName(release string) string        { return release + "-platform" }

func validateGitOpsPlan(plan map[string][]byte, resolved gitOpsResolved) error {
	for name, data := range plan {
		if filepath.IsAbs(name) || !pathWithin(".", filepath.Clean(name)) {
			return fmt.Errorf("generated path %q is unsafe", name)
		}
		if bytes.Contains(data, []byte("kind: Secret")) {
			return fmt.Errorf("generated file %s contains a Secret object", name)
		}
		if bytes.Contains(data, []byte(resolved.ProjectDir)) {
			return fmt.Errorf("generated file %s contains the local checkout path", name)
		}
		if err := rejectCredentialMaterial("generated file "+name, data); err != nil {
			return err
		}
	}
	wantPlatform := []string{"platform/values-configmap.yaml", "platform/helmrelease.yaml", "platform/kustomization.yaml", "sources/platform-chart.yaml"}
	for _, name := range wantPlatform {
		_, present := plan[name]
		if present != resolved.FixEnabled {
			return fmt.Errorf("generated platform file set does not match Agent Sandbox Fix selection")
		}
	}
	return nil
}

type gitOpsFileAction struct {
	Kind   string
	Path   string
	Digest string
}

func (action gitOpsFileAction) String() string { return action.Kind + " " + action.Path }

func planGitOpsWrites(projectDir, outputDir string, plan map[string][]byte) ([]gitOpsFileAction, error) {
	root, exists, err := openGitOpsOutput(projectDir, outputDir, false)
	if err != nil {
		return nil, err
	}
	if root != nil {
		defer root.Close()
	}
	var actions []gitOpsFileAction
	for _, name := range sortedGitOpsPaths(plan) {
		action := gitOpsFileAction{Kind: "create", Path: name}
		if exists {
			if err := inspectRootParents(root, name); err != nil {
				return nil, err
			}
			info, err := root.Lstat(name)
			switch {
			case err == nil:
				if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
					return nil, fmt.Errorf("generated file path %s conflicts with a non-regular filesystem entry", name)
				}
				current, digest, err := readRootFile(root, name)
				if err != nil {
					return nil, err
				}
				action.Digest = digest
				if bytes.Equal(current, plan[name]) {
					action.Kind = "unchanged"
				} else {
					action.Kind = "replace"
				}
			case os.IsNotExist(err):
			default:
				return nil, fmt.Errorf("inspect generated file %s: %w", name, err)
			}
		}
		actions = append(actions, action)
	}
	if exists {
		for _, name := range obsoleteGitOpsFiles(plan) {
			if err := inspectRootParents(root, name); err != nil {
				return nil, err
			}
			info, err := root.Lstat(name)
			switch {
			case err == nil:
				if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
					return nil, fmt.Errorf("obsolete generated path %s is not a regular file", name)
				}
				_, digest, err := readRootFile(root, name)
				if err != nil {
					return nil, err
				}
				actions = append(actions, gitOpsFileAction{Kind: "remove", Path: name, Digest: digest})
			case os.IsNotExist(err):
			default:
				return nil, fmt.Errorf("inspect obsolete generated file %s: %w", name, err)
			}
		}
	}
	return actions, nil
}

func obsoleteGitOpsFiles(plan map[string][]byte) []string {
	var obsolete []string
	for _, name := range []string{"platform/values-configmap.yaml", "platform/helmrelease.yaml", "platform/kustomization.yaml", "sources/platform-chart.yaml"} {
		if _, expected := plan[name]; !expected {
			obsolete = append(obsolete, name)
		}
	}
	return obsolete
}

func applyGitOpsWrites(projectDir, outputDir string, plan map[string][]byte, actions []gitOpsFileAction) error {
	root, _, err := openGitOpsOutput(projectDir, outputDir, true)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, action := range actions {
		if action.Kind == "unchanged" {
			continue
		}
		if err := ensureRootParents(root, action.Path); err != nil {
			return err
		}
		info, statErr := root.Lstat(action.Path)
		switch action.Kind {
		case "create":
			if !os.IsNotExist(statErr) {
				return fmt.Errorf("generated directory changed after planning; %s is no longer absent", action.Path)
			}
			if err := writeRootFile(root, action.Path, plan[action.Path], false); err != nil {
				return fmt.Errorf("create %s: %w", action.Path, err)
			}
		case "replace", "remove":
			if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return fmt.Errorf("generated directory changed after planning; %s is no longer a regular file", action.Path)
			}
			_, digest, err := readRootFile(root, action.Path)
			if err != nil || digest != action.Digest {
				return fmt.Errorf("generated directory changed after planning; %s content no longer matches", action.Path)
			}
			if action.Kind == "remove" {
				if err := root.Remove(action.Path); err != nil {
					return fmt.Errorf("remove %s: %w", action.Path, err)
				}
				continue
			}
			if err := writeRootFile(root, action.Path, plan[action.Path], true); err != nil {
				return fmt.Errorf("replace %s: %w", action.Path, err)
			}
		}
	}
	return nil
}

func openGitOpsOutput(projectDir, outputDir string, create bool) (*os.Root, bool, error) {
	rel, err := filepath.Rel(projectDir, outputDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, false, fmt.Errorf("GitOps output must be a child directory of the consumer repository")
	}
	rel = filepath.ToSlash(rel)
	projectRoot, err := os.OpenRoot(projectDir)
	if err != nil {
		return nil, false, fmt.Errorf("open consumer repository: %w", err)
	}
	defer projectRoot.Close()
	info, err := projectRoot.Lstat(rel)
	if os.IsNotExist(err) && create {
		if err := projectRoot.MkdirAll(rel, 0o755); err != nil {
			return nil, false, fmt.Errorf("create GitOps output directory: %w", err)
		}
		info, err = projectRoot.Lstat(rel)
	}
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect GitOps output directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, false, fmt.Errorf("GitOps output path conflicts with a non-directory filesystem entry")
	}
	root, err := os.OpenRoot(outputDir)
	if err != nil {
		return nil, false, fmt.Errorf("open GitOps output directory: %w", err)
	}
	return root, true, nil
}

func inspectRootParents(root *os.Root, filename string) error {
	parent := path.Dir(filename)
	if parent == "." {
		return nil
	}
	current := ""
	for _, part := range strings.Split(parent, "/") {
		current = path.Join(current, part)
		info, err := root.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect generated directory %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("generated directory %s conflicts with a non-directory filesystem entry", current)
		}
	}
	return nil
}

func ensureRootParents(root *os.Root, filename string) error {
	parent := path.Dir(filename)
	if parent == "." {
		return nil
	}
	if err := root.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create generated directory %s: %w", parent, err)
	}
	return inspectRootParents(root, filename)
}

func readRootFile(root *os.Root, filename string) ([]byte, string, error) {
	data, err := root.ReadFile(filename)
	if err != nil {
		return nil, "", fmt.Errorf("read generated file %s: %w", filename, err)
	}
	digest := sha256.Sum256(data)
	return data, "sha256:" + hex.EncodeToString(digest[:]), nil
}

func writeRootFile(root *os.Root, filename string, data []byte, replace bool) error {
	if !replace {
		file, err := root.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		if _, err := file.Write(data); err != nil {
			file.Close()
			return err
		}
		return file.Close()
	}
	parent := path.Dir(filename)
	for attempt := 0; attempt < 10; attempt++ {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return err
		}
		tempName := path.Join(parent, fmt.Sprintf(".aster-gitops-%x", suffix[:]))
		temp, err := root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		cleanup := true
		defer func() {
			_ = temp.Close()
			if cleanup {
				_ = root.Remove(tempName)
			}
		}()
		if _, err := temp.Write(data); err != nil {
			return err
		}
		if err := temp.Sync(); err != nil {
			return err
		}
		if err := temp.Close(); err != nil {
			return err
		}
		if err := root.Rename(tempName, filename); err != nil {
			return err
		}
		cleanup = false
		return nil
	}
	return fmt.Errorf("could not allocate a temporary generated file")
}

func readSafeGeneratedFile(projectDir, outputDir, name string) ([]byte, error) {
	root, exists, err := openGitOpsOutput(projectDir, outputDir, false)
	if err != nil || !exists {
		if err != nil {
			return nil, err
		}
		return nil, os.ErrNotExist
	}
	defer root.Close()
	if err := inspectRootParents(root, name); err != nil {
		return nil, err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("generated path %s is not a regular file", name)
	}
	data, _, err := readRootFile(root, name)
	return data, err
}

func listGitOpsFiles(projectDir, outputDir string) ([]string, error) {
	root, exists, err := openGitOpsOutput(projectDir, outputDir, false)
	if err != nil || !exists {
		return nil, err
	}
	defer root.Close()
	var names []string
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("GitOps directory contains symlink %s", name)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("GitOps directory contains non-regular file %s", name)
		}
		names = append(names, filepath.ToSlash(name))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inspect GitOps directory: %w", err)
	}
	sort.Strings(names)
	return names, nil
}

func sortedGitOpsPaths(plan map[string][]byte) []string {
	paths := make([]string, 0, len(plan))
	for path := range plan {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func displayProjectPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return filepath.Base(path)
}

const gitOpsReadme = `# Generated Aster Flux bundle

This directory is generated by Aster. Edit project.yaml, prompts/system.md,
skills/*.yaml, or deploy values, then rerun gitops render and gitops check.

The bundle uses standard Flux resources. Values ConfigMaps contain project and
prompt data but no Secret values, so keep repository access reviewed and scoped.
Provision runtime Secrets separately. Roll back by reverting and merging the
reviewed deployment commit so Flux reconciles the previous declaration.
`
