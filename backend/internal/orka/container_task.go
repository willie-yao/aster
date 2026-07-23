package orka

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const (
	// ContainerAnalysisContractVersion identifies the experimental adapter contract.
	ContainerAnalysisContractVersion = "dashboard-failure-analyzer-v1"
	containerResultMaxBytes          = 2 << 20
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
	Namespace       string
	NamePrefix      string
	Image           string
	Command         []string
	Args            []string
	Timeout         string
	MaxRetries      int
	ContractVersion string
	Request         ai.FailureAnalysisRequest
	Environment     map[string]string
	SecretEnv       []SecretEnvVar
	Labels          map[string]string
}

// BuildContainerAnalysisTask builds one content-addressed Orka container Task.
func BuildContainerAnalysisTask(in ContainerAnalysisTaskSpec) (map[string]any, error) {
	if strings.TrimSpace(in.Namespace) == "" {
		return nil, fmt.Errorf("container analysis Task namespace is required")
	}
	if strings.TrimSpace(in.Image) == "" {
		return nil, fmt.Errorf("container analysis Task image is required")
	}
	if len(in.Command) == 0 {
		return nil, fmt.Errorf("container analysis Task command is required")
	}
	if strings.TrimSpace(in.Timeout) == "" {
		return nil, fmt.Errorf("container analysis Task timeout is required")
	}
	if in.MaxRetries < 0 {
		return nil, fmt.Errorf("container analysis Task retries must not be negative")
	}
	if in.ContractVersion == "" {
		in.ContractVersion = ContainerAnalysisContractVersion
	}
	requestJSON, requestDigest, err := analysisruntime.EncodeInlineRequest(in.Request)
	if err != nil {
		return nil, err
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
		analysisruntime.InlineRequestEnv:       true,
		analysisruntime.InlineRequestDigestEnv: true,
	}
	for name := range in.Environment {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("container analysis Task environment name is required")
		}
		if !safeInlineEnvironmentName(name) {
			return nil, fmt.Errorf("container analysis Task environment %s must use a Secret reference", name)
		}
		if seenEnv[name] {
			return nil, fmt.Errorf("container analysis Task environment must not override %s", name)
		}
		seenEnv[name] = true
	}
	for _, secret := range secretEnv {
		if strings.TrimSpace(secret.Name) == "" || strings.TrimSpace(secret.SecretName) == "" || strings.TrimSpace(secret.SecretKey) == "" {
			return nil, fmt.Errorf("container analysis Task secret environment references require name, Secret name, and key")
		}
		if seenEnv[secret.Name] {
			return nil, fmt.Errorf("container analysis Task environment contains duplicate %s", secret.Name)
		}
		seenEnv[secret.Name] = true
	}
	in.SecretEnv = secretEnv

	name, err := containerAnalysisTaskName(in, requestDigest)
	if err != nil {
		return nil, err
	}
	env := []any{
		map[string]any{"name": analysisruntime.InlineRequestEnv, "value": string(requestJSON)},
		map[string]any{"name": analysisruntime.InlineRequestDigestEnv, "value": requestDigest},
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
	labels := map[string]any{
		"app.kubernetes.io/managed-by": "prow-ai-dashboard",
		"prow-ai-dashboard/adapter":    "container-analyzer",
	}
	for key, value := range in.Labels {
		labels[key] = value
	}
	return map[string]any{
		"apiVersion": "core.orka.ai/v1alpha1",
		"kind":       "Task",
		"metadata": map[string]any{
			"name":      name,
			"namespace": in.Namespace,
			"labels":    labels,
			"annotations": map[string]any{
				"prow-ai-dashboard/request-digest":   requestDigest,
				"prow-ai-dashboard/contract-version": in.ContractVersion,
			},
		},
		"spec": map[string]any{
			"type":        "container",
			"image":       in.Image,
			"command":     append([]string(nil), in.Command...),
			"args":        append([]string(nil), in.Args...),
			"env":         env,
			"timeout":     in.Timeout,
			"retryPolicy": map[string]any{"maxRetries": in.MaxRetries},
			"execution": map[string]any{
				"nodeSelector": map[string]any{"agentpool": "nodepool1"},
			},
		},
	}, nil
}

func safeInlineEnvironmentName(name string) bool {
	switch name {
	case "AI_API", "AI_ENDPOINT", "AI_MODEL":
		return true
	default:
		return false
	}
}

func containerAnalysisTaskName(in ContainerAnalysisTaskSpec, requestDigest string) (string, error) {
	identity := struct {
		RequestDigest   string
		Image           string
		Command         []string
		Args            []string
		Timeout         string
		MaxRetries      int
		ContractVersion string
		Environment     map[string]string
		SecretEnv       []SecretEnvVar
	}{requestDigest, in.Image, in.Command, in.Args, in.Timeout, in.MaxRetries, in.ContractVersion, in.Environment, in.SecretEnv}
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

// ParseContainerAnalysisResult extracts the analyzer's final JSON line from the
// combined pod log result stored by the pinned Orka controller.
func ParseContainerAnalysisResult(raw string) (ai.FailureAnalysisResult, error) {
	var result ai.FailureAnalysisResult
	if len(raw) > containerResultMaxBytes {
		return result, fmt.Errorf("container analysis result exceeds %d bytes", containerResultMaxBytes)
	}
	lines := strings.Split(raw, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&result); err != nil {
			return result, fmt.Errorf("decode final container analysis result line: %w", err)
		}
		var extra any
		if err := decoder.Decode(&extra); err == nil {
			return result, fmt.Errorf("container analysis result line contains multiple JSON values")
		} else if err != io.EOF {
			return result, fmt.Errorf("decode trailing container analysis result data: %w", err)
		}
		if result.Summary == nil {
			return result, fmt.Errorf("container analysis result has no ai_summary")
		}
		if strings.TrimSpace(result.Summary.Summary) == "" {
			return result, fmt.Errorf("container analysis result has an empty ai_summary.summary")
		}
		return result, nil
	}
	return result, fmt.Errorf("container analysis result is empty")
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
