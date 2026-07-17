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

// AnalysisContract is the model and tool contract that determines whether a
// prior Orka result can be reused.
type AnalysisContract struct {
	Provider     string         `json:"provider"`
	Model        string         `json:"model"`
	Version      string         `json:"version"`
	Timeout      string         `json:"timeout"`
	Retries      int            `json:"retries"`
	SystemPrompt string         `json:"system_prompt"`
	Tools        []ToolContract `json:"tools"`
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

// FailurePrompt renders the per-test prompt shared by the producer and ingestor.
func FailurePrompt(projectLabel, jobID, buildPrefix string, tc models.TestCase) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This %s CI test FAILED. Root-cause it and classify transient vs real bug.\n\n", projectLabel)
	fmt.Fprintf(&b, "Job: %s\n", jobID)
	fmt.Fprintf(&b, "Build: %s\n", buildPrefix)
	fmt.Fprintf(&b, "Failed test: %s\n", tc.Name)
	if tc.FailureLocation != "" {
		fmt.Fprintf(&b, "Failure location: %s\n", tc.FailureLocation)
	}
	if tc.JUnitFile != "" {
		fmt.Fprintf(&b, "JUnit file: %s\n", tc.JUnitFile)
	}
	msg := tc.FailureMessage
	if len(msg) > 1200 {
		msg = msg[:1200]
	}
	if msg != "" {
		fmt.Fprintf(&b, "Failure output:\n%s\n", msg)
	}
	b.WriteString("\nInvestigate the build's artifacts with the tools and conclude with your JSON.")
	return b.String()
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		fmt.Fprintf(h, "%d:", len(part))
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil)[:identityDigestBytes])
}
