package orka

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

const identityDigestBytes = 16

// AcceptanceVersion identifies the Orka result acceptance contract.
const AcceptanceVersion = 7

// ToolScopeHeader binds artifact-tool caches and budgets to one Tool contract.
const ToolScopeHeader = "X-Prow-AI-Scope"

// ValidationKeyHeader carries the private result-attestation key to validate_analysis.
const ValidationKeyHeader = "X-Prow-AI-Validation-Key"

// MinGCSBytesHeader carries the configured artifact-read floor to validate_analysis.
const MinGCSBytesHeader = "X-Prow-AI-Min-GCS-Bytes"

// ValidationTaskHeader binds validate_analysis to one analysis Task.
const ValidationTaskHeader = "X-Prow-AI-Task"

// AnalysisContract is the model and tool contract that determines whether a
// prior Orka result can be reused.
type AnalysisContract struct {
	Provider          string         `json:"provider"`
	Model             string         `json:"model"`
	APIMode           string         `json:"api_mode"`
	Version           string         `json:"version"`
	Timeout           string         `json:"timeout"`
	Retries           int            `json:"retries"`
	MinToolCalls      int            `json:"min_tool_calls"`
	MinGCSBytes       int            `json:"min_gcs_bytes"`
	AcceptanceVersion int            `json:"acceptance_version"`
	SkillSetHash      string         `json:"skill_set_hash,omitempty"`
	ToolAuthSecret    string         `json:"tool_auth_secret,omitempty"`
	ToolAuthKey       string         `json:"tool_auth_key,omitempty"`
	ValidationKeyHash string         `json:"validation_key_hash"`
	SystemPrompt      string         `json:"system_prompt"`
	Tools             []ToolContract `json:"tools"`
}

// ToolContract is one enabled Orka Tool definition in model-visible order.
type ToolContract struct {
	Name       string         `json:"name"`
	Definition map[string]any `json:"definition"`
}

// AnalysisContractHash fingerprints the complete model-visible analysis contract.
func AnalysisContractHash(contract AnalysisContract) (string, error) {
	data, err := json.Marshal(contract)
	if err != nil {
		return "", fmt.Errorf("marshal analysis contract: %w", err)
	}
	return digest(string(data)), nil
}

// ProjectScopeID separates consumers that share one Orka namespace.
func ProjectScopeID(projectID, storageProvider, bucket, storageBase, webBase, prowBase string) string {
	return digest(projectID, storageProvider, bucket, storageBase, webBase, prowBase)
}

// BuildKey is the manifest lookup key for one job build.
func BuildKey(jobID, buildID string) string {
	return digest(jobID, buildID)
}

// BuildScopeID identifies one consumer, job, and artifact build route.
func BuildScopeID(projectScope, jobID, buildID, buildPrefix string) string {
	return digest(projectScope, jobID, buildID, buildPrefix)
}

// ToolScopeID versions a build's Tool resources by their analysis contract.
func ToolScopeID(buildScope, contractHash string) string {
	return digest(buildScope, contractHash)
}

// AnalysisTaskName identifies one exact failure under one analysis contract.
func AnalysisTaskName(projectScope, buildScope, contractHash string, testIndex int, prompt string) string {
	hash := digest(projectScope, buildScope, contractHash, strconv.Itoa(testIndex), prompt)
	return Sanitize("az-analysis-" + hash)
}

const (
	failureMessagePromptBytes    = 16 * 1024
	failureBodyPromptBytes       = 8 * 1024
	persistentFailurePromptFloor = 3
)

// FailurePrompt renders the per-test prompt shared by the producer and ingestor.
func FailurePrompt(projectLabel, jobID, buildPrefix string, tc models.TestCase, consecutiveFailures int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This %s CI test FAILED. Root-cause it and classify transient vs real bug.\n\n", projectLabel)
	fmt.Fprintf(&b, "Job: %s\n", jobID)
	fmt.Fprintf(&b, "Build: %s\n", buildPrefix)
	fmt.Fprintf(&b, "Failed test: %s\n", tc.Name)
	if consecutiveFailures >= persistentFailurePromptFloor {
		fmt.Fprintf(&b, "Consecutive failures on this test: at least %d (persistent, not flaky).\n", persistentFailurePromptFloor)
	} else if consecutiveFailures > 1 {
		fmt.Fprintf(&b, "Consecutive failures on this test: %d.\n", consecutiveFailures)
	}
	if tc.FailureLocation != "" {
		fmt.Fprintf(&b, "Failure location: %s\n", tc.FailureLocation)
	}
	if tc.JUnitFile != "" {
		fmt.Fprintf(&b, "JUnit file: %s\n", tc.JUnitFile)
	}
	message := strings.TrimSpace(tc.FailureMessage)
	body := strings.TrimSpace(tc.FailureBody)
	if message != "" || body != "" {
		b.WriteString("\nDeterministic pre-triage evidence:\n")
	}
	if message != "" {
		b.WriteString("Failure message:\n")
		b.WriteString(clampPromptHeadTail(message, failureMessagePromptBytes))
		b.WriteByte('\n')
	}
	if body != "" {
		b.WriteString("Failure body (truncated to last 8KB):\n")
		if len(body) > failureBodyPromptBytes {
			body = strings.ToValidUTF8(body[len(body)-failureBodyPromptBytes:], "")
		}
		b.WriteString(body)
		b.WriteByte('\n')
	}
	b.WriteString("\nInvestigate the build's artifacts with the tools and conclude with your JSON.")
	return b.String()
}

func clampPromptHeadTail(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	headBytes := maxBytes * 3 / 4
	tailBytes := maxBytes - headBytes
	head := strings.ToValidUTF8(value[:headBytes], "")
	tail := strings.ToValidUTF8(value[len(value)-tailBytes:], "")
	return head + fmt.Sprintf("\n... [%d bytes elided; read the JUnit artifact for the complete failure] ...\n", len(value)-len(head)-len(tail)) + tail
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		fmt.Fprintf(h, "%d:", len(part))
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil)[:identityDigestBytes])
}
