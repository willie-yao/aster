package orka

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

const (
	// ContainerAnalysisContractVersion identifies the experimental adapter contract.
	ContainerAnalysisContractVersion = analysisruntime.ContainerAnalyzerContractVersion
)

var invalidTaskNameChars = regexp.MustCompile(`[^a-z0-9-]+`)

// SecretEnvVar maps one analyzer environment variable to a Kubernetes Secret key.
type SecretEnvVar struct {
	Name       string
	SecretName string
	SecretKey  string
}

// ContainerAnalysisTaskSpec is the dashboard-owned container Task contract.
type ContainerAnalysisTaskSpec struct {
	Namespace   string
	NamePrefix  string
	Image       string
	Command     []string
	Args        []string
	Timeout     string
	MaxRetries  int
	ProjectDir  string
	Request     ai.FailureAnalysisRequest
	CacheSeed   map[string]ai.CacheEntry
	Environment map[string]string
	SecretEnv   []SecretEnvVar
	Labels      map[string]string
}

// ContainerAnalysisResources are the immutable bundle and its Orka Task.
type ContainerAnalysisResources struct {
	BundleConfigMap map[string]any
	Task            map[string]any
}

// BuildContainerAnalysisResources builds one content-addressed bundle and Task.
func BuildContainerAnalysisResources(in ContainerAnalysisTaskSpec) (ContainerAnalysisResources, error) {
	if strings.TrimSpace(in.Namespace) == "" {
		return ContainerAnalysisResources{}, fmt.Errorf("container analysis Task namespace is required")
	}
	if strings.TrimSpace(in.Image) == "" {
		return ContainerAnalysisResources{}, fmt.Errorf("container analysis Task image is required")
	}
	if strings.TrimSpace(in.Timeout) == "" {
		return ContainerAnalysisResources{}, fmt.Errorf("container analysis Task timeout is required")
	}
	if in.MaxRetries < 0 {
		return ContainerAnalysisResources{}, fmt.Errorf("container analysis Task retries must not be negative")
	}
	if strings.TrimSpace(in.ProjectDir) == "" {
		return ContainerAnalysisResources{}, fmt.Errorf("container analysis project directory is required")
	}
	environment := maps.Clone(in.Environment)
	api := strings.ToLower(strings.TrimSpace(environment["AI_API"]))
	if api == "" {
		return ContainerAnalysisResources{}, fmt.Errorf("container analysis Task environment requires AI_API")
	}
	if err := project.ValidateAIAPI(api); err != nil {
		return ContainerAnalysisResources{}, err
	}
	endpoint, err := validateContainerAnalysisEndpoint(environment["AI_ENDPOINT"])
	if err != nil {
		return ContainerAnalysisResources{}, err
	}
	model := strings.TrimSpace(environment["AI_MODEL"])
	if model == "" {
		return ContainerAnalysisResources{}, fmt.Errorf("container analysis Task environment requires AI_MODEL")
	}
	environment["AI_API"] = api
	environment["AI_ENDPOINT"] = endpoint
	environment["AI_MODEL"] = model
	in.Environment = environment
	bundleJSON, bundleDigest, err := analysisruntime.BuildProjectBundleWithCache(in.ProjectDir, ContainerAnalysisContractVersion, in.Request, in.CacheSeed)
	if err != nil {
		return ContainerAnalysisResources{}, err
	}
	secretEnv := append([]SecretEnvVar(nil), in.SecretEnv...)
	sort.Slice(secretEnv, func(i, j int) bool {
		if secretEnv[i].Name != secretEnv[j].Name {
			return secretEnv[i].Name < secretEnv[j].Name
		}
		if secretEnv[i].SecretName != secretEnv[j].SecretName {
			return secretEnv[i].SecretName < secretEnv[j].SecretName
		}
		return secretEnv[i].SecretKey < secretEnv[j].SecretKey
	})
	seenEnv := map[string]bool{
		analysisruntime.ProjectBundleEnv:            true,
		analysisruntime.ProjectBundleDigestEnv:      true,
		analysisruntime.ContainerTaskNamespaceEnv:   true,
		analysisruntime.ContainerTaskNameEnv:        true,
		analysisruntime.ContainerContractVersionEnv: true,
	}
	for name := range in.Environment {
		if strings.TrimSpace(name) == "" {
			return ContainerAnalysisResources{}, fmt.Errorf("container analysis Task environment name is required")
		}
		if !safeInlineEnvironmentName(name) {
			return ContainerAnalysisResources{}, fmt.Errorf("container analysis Task environment %s must use a Secret reference", name)
		}
		if seenEnv[name] {
			return ContainerAnalysisResources{}, fmt.Errorf("container analysis Task environment must not override %s", name)
		}
		seenEnv[name] = true
	}
	tokenSecretFound := false
	stateSecretFound := false
	for _, secret := range secretEnv {
		if strings.TrimSpace(secret.Name) == "" || strings.TrimSpace(secret.SecretName) == "" || strings.TrimSpace(secret.SecretKey) == "" {
			return ContainerAnalysisResources{}, fmt.Errorf("container analysis Task secret environment references require name, Secret name, and key")
		}
		if seenEnv[secret.Name] {
			return ContainerAnalysisResources{}, fmt.Errorf("container analysis Task environment contains duplicate %s", secret.Name)
		}
		seenEnv[secret.Name] = true
		if secret.Name == "AI_TOKEN" {
			tokenSecretFound = true
		}
		if secret.Name == analysisruntime.ContainerStateKeyEnv {
			stateSecretFound = true
		}
	}
	if !tokenSecretFound {
		return ContainerAnalysisResources{}, fmt.Errorf("container analysis Task requires an AI_TOKEN Secret reference")
	}
	if !stateSecretFound {
		return ContainerAnalysisResources{}, fmt.Errorf("container analysis Task requires a %s Secret reference", analysisruntime.ContainerStateKeyEnv)
	}
	in.SecretEnv = secretEnv

	name, err := containerAnalysisTaskName(in, bundleDigest)
	if err != nil {
		return ContainerAnalysisResources{}, err
	}
	bundleName := containerAnalysisBundleName(name)
	env := []any{
		map[string]any{
			"name": analysisruntime.ProjectBundleEnv,
			"valueFrom": map[string]any{
				"configMapKeyRef": map[string]any{"name": bundleName, "key": analysisruntime.ProjectBundleConfigMapKey},
			},
		},
		map[string]any{"name": analysisruntime.ProjectBundleDigestEnv, "value": bundleDigest},
		map[string]any{"name": analysisruntime.ContainerTaskNamespaceEnv, "value": in.Namespace},
		map[string]any{"name": analysisruntime.ContainerTaskNameEnv, "value": name},
		map[string]any{"name": analysisruntime.ContainerContractVersionEnv, "value": ContainerAnalysisContractVersion},
	}
	environmentNames := make([]string, 0, len(in.Environment))
	for name := range in.Environment {
		environmentNames = append(environmentNames, name)
	}
	sort.Strings(environmentNames)
	for _, name := range environmentNames {
		env = append(env, map[string]any{"name": name, "value": in.Environment[name]})
	}
	for _, secret := range secretEnv {
		env = append(env, map[string]any{
			"name": secret.Name,
			"valueFrom": map[string]any{
				"secretKeyRef": map[string]any{"name": secret.SecretName, "key": secret.SecretKey},
			},
		})
	}
	bundleLabels := containerAnalysisLabels(in.Labels)
	bundleLabels[containerAnalysisBundleLabel] = "true"
	bundleAnnotations := containerAnalysisAnnotations(ContainerAnalysisContractVersion, bundleDigest)
	bundleAnnotations[containerAnalysisTaskNameAnnotation] = name
	taskLabels := containerAnalysisLabels(in.Labels)
	containerSpec := map[string]any{
		"type":        "container",
		"image":       in.Image,
		"args":        append([]string(nil), in.Args...),
		"env":         env,
		"timeout":     in.Timeout,
		"retryPolicy": map[string]any{"maxRetries": in.MaxRetries},
		"execution": map[string]any{
			"nodeSelector": map[string]any{"agentpool": "nodepool1"},
		},
	}
	if len(in.Command) > 0 {
		containerSpec["command"] = append([]string(nil), in.Command...)
	}
	return ContainerAnalysisResources{
		BundleConfigMap: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name": bundleName, "namespace": in.Namespace, "labels": bundleLabels, "annotations": bundleAnnotations,
			},
			"immutable": true,
			"data":      map[string]any{analysisruntime.ProjectBundleConfigMapKey: string(bundleJSON)},
		},
		Task: map[string]any{
			"apiVersion": "core.orka.ai/v1alpha1",
			"kind":       "Task",
			"metadata": map[string]any{
				"name": name, "namespace": in.Namespace, "labels": taskLabels, "annotations": containerAnalysisAnnotations(ContainerAnalysisContractVersion, bundleDigest),
			},
			"spec": containerSpec,
		},
	}, nil
}

func containerAnalysisAnnotations(contractVersion, bundleDigest string) map[string]any {
	return map[string]any{
		"prow-ai-dashboard/bundle-digest":    bundleDigest,
		"prow-ai-dashboard/contract-version": contractVersion,
	}
}

func containerAnalysisLabels(extra map[string]string) map[string]any {
	labels := make(map[string]any, len(extra)+2)
	for key, value := range extra {
		labels[key] = value
	}
	labels["app.kubernetes.io/managed-by"] = "prow-ai-dashboard"
	labels["prow-ai-dashboard/adapter"] = "container-analyzer"
	return labels
}

func containerAnalysisBundleName(taskName string) string {
	return taskName + "-input"
}

func validateContainerAnalysisEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("container analysis Task environment requires AI_ENDPOINT")
	}
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return "", fmt.Errorf("container analysis Task AI_ENDPOINT must be an absolute http or https URL")
	}
	if endpoint.User != nil {
		return "", fmt.Errorf("container analysis Task AI_ENDPOINT must not contain URL credentials")
	}
	if endpoint.Fragment != "" {
		return "", fmt.Errorf("container analysis Task AI_ENDPOINT must not contain a fragment")
	}
	query, err := url.ParseQuery(endpoint.RawQuery)
	if err != nil {
		return "", fmt.Errorf("container analysis Task AI_ENDPOINT has an invalid query")
	}
	for key, values := range query {
		if !strings.EqualFold(key, "api-version") || len(values) != 1 || strings.TrimSpace(values[0]) == "" {
			return "", fmt.Errorf("container analysis Task AI_ENDPOINT allows only a non-empty api-version query parameter")
		}
	}
	return raw, nil
}

func safeInlineEnvironmentName(name string) bool {
	switch name {
	case "AI_API", "AI_ENDPOINT", "AI_MODEL":
		return true
	default:
		return false
	}
}

func containerAnalysisTaskName(in ContainerAnalysisTaskSpec, bundleDigest string) (string, error) {
	identity := struct {
		BundleDigest string
		Image        string
		Command      []string
		Args         []string
		Timeout      string
		MaxRetries   int
		Environment  map[string]string
		SecretEnv    []SecretEnvVar
	}{bundleDigest, in.Image, in.Command, in.Args, in.Timeout, in.MaxRetries, in.Environment, in.SecretEnv}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("marshal container analysis Task identity: %w", err)
	}
	sum := sha256.Sum256(data)
	prefix := strings.Trim(invalidTaskNameChars.ReplaceAllString(strings.ToLower(in.NamePrefix), "-"), "-")
	if prefix == "" {
		prefix = "dashboard-analyzer"
	}
	if len(prefix) > 40 {
		prefix = strings.TrimRight(prefix[:40], "-")
	}
	return prefix + "-" + hex.EncodeToString(sum[:8]), nil
}

// ParseContainerAnalysisResult extracts the dashboard-owned result marker from
// the combined pod logs stored by the Orka controller.
func ParseContainerAnalysisResult(raw string) (ai.FailureAnalysisResult, error) {
	result, err := analysisruntime.ParseFailureAnalysisResult(raw)
	if err != nil {
		return result, fmt.Errorf("parse container analysis result: %w", err)
	}
	return result, nil
}

// ApplyContainerAnalysisResult maps the authoritative dashboard result onto a test case.
func ApplyContainerAnalysisResult(tc *models.TestCase, result ai.FailureAnalysisResult) error {
	if tc == nil {
		return fmt.Errorf("test case is required")
	}
	if result.Summary == nil {
		return fmt.Errorf("container analysis result has no ai_summary")
	}
	if strings.TrimSpace(result.Summary.Summary) == "" {
		return fmt.Errorf("container analysis result has an empty ai_summary.summary")
	}
	tc.AISummary = result.Summary
	tc.AIAnalysis = result.Analysis
	return nil
}
