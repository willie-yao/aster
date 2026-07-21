package orka

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

// AnalysisManifestFile is the private producer-to-ingestor identity contract.
const AnalysisManifestFile = "orka_analysis.json"

const analysisManifestVersion = 4

// AnalysisManifest records the exact Task identity contract for one fetch pass.
type AnalysisManifest struct {
	SchemaVersion int                      `json:"schema_version"`
	ProjectScope  string                   `json:"project_scope"`
	ProjectLabel  string                   `json:"project_label"`
	ContractHash  string                   `json:"contract_hash"`
	Provider      string                   `json:"provider"`
	Model         string                   `json:"model"`
	Version       string                   `json:"version"`
	MinToolCalls  int                      `json:"min_tool_calls"`
	MinGCSBytes   int                      `json:"min_gcs_bytes"`
	SkillSetHash  string                   `json:"skill_set_hash,omitempty"`
	ValidationKey string                   `json:"validation_key"`
	Jobs          map[string]bool          `json:"jobs"`
	Builds        map[string]AnalysisBuild `json:"builds"`
}

// AnalysisBuild holds the content-addressed Tool scope for one job build.
type AnalysisBuild struct {
	BuildScope     string `json:"build_scope"`
	ToolScope      string `json:"tool_scope"`
	Prefix         string `json:"prefix"`
	PromptSeedHash string `json:"prompt_seed_hash,omitempty"`
}

// AnalysisTaskRef is the Task and Tool identity for one failed test.
type AnalysisTaskRef struct {
	Name      string
	ToolScope string
	Prompt    string
}

// NewAnalysisManifest constructs an empty manifest for one producer pass.
func NewAnalysisManifest(projectScope, projectLabel, contractHash, provider, model, version string, minToolCalls int) *AnalysisManifest {
	return &AnalysisManifest{
		SchemaVersion: analysisManifestVersion,
		ProjectScope:  projectScope,
		ProjectLabel:  projectLabel,
		ContractHash:  contractHash,
		Provider:      provider,
		Model:         model,
		Version:       version,
		MinToolCalls:  minToolCalls,
		Jobs:          map[string]bool{},
		Builds:        map[string]AnalysisBuild{},
	}
}

// SetBuild records the build scope needed to re-derive Task names.
func (m *AnalysisManifest) SetBuild(jobID, buildID, buildScope, toolScope, prefix, promptSeed string) {
	promptSeedHash := ""
	if promptSeed != "" {
		promptSeedHash = digest(promptSeed)
	}
	m.Jobs[jobID] = true
	m.Builds[BuildKey(jobID, buildID)] = AnalysisBuild{
		BuildScope: buildScope, ToolScope: toolScope, Prefix: prefix, PromptSeedHash: promptSeedHash,
	}
}

// TaskRef re-derives the exact Task name emitted by the producer.
func (m *AnalysisManifest) TaskRef(jobID string, run models.BuildResult, testIndex int, tc models.TestCase) (AnalysisTaskRef, error) {
	build, ok := m.Builds[BuildKey(jobID, run.BuildID)]
	if !ok {
		return AnalysisTaskRef{}, fmt.Errorf("build identity not found for %s/%s", jobID, run.BuildID)
	}
	prompt := FailurePrompt(m.ProjectLabel, jobID, build.Prefix, tc)
	identityPrompt := prompt
	if build.PromptSeedHash != "" {
		identityPrompt += "\nartifact-tree-seed:" + build.PromptSeedHash
	}
	return AnalysisTaskRef{
		Name:      AnalysisTaskName(m.ProjectScope, build.BuildScope, m.ContractHash, testIndex, identityPrompt),
		ToolScope: build.ToolScope,
		Prompt:    prompt,
	}, nil
}

// Write saves the manifest atomically beside the dashboard data.
func (m *AnalysisManifest) Write(dataDir string) error {
	if err := m.Validate(); err != nil {
		return err
	}
	return statefile.WriteJSON(filepath.Join(dataDir, AnalysisManifestFile), m)
}

// LoadAnalysisManifest reads and validates the producer identity contract.
func LoadAnalysisManifest(dataDir string) (*AnalysisManifest, error) {
	path := filepath.Join(dataDir, AnalysisManifestFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Orka analysis manifest: %w", err)
	}
	var manifest AnalysisManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse Orka analysis manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return &manifest, nil
}

// Validate checks the required producer-to-ingestor identity fields.
func (m *AnalysisManifest) Validate() error {
	if m.SchemaVersion != analysisManifestVersion {
		return fmt.Errorf("unsupported Orka analysis manifest version %d", m.SchemaVersion)
	}
	if m.ProjectScope == "" || m.ContractHash == "" || m.Provider == "" || m.Model == "" || m.Version == "" || m.ValidationKey == "" {
		return fmt.Errorf("orka analysis manifest is missing required identity fields")
	}
	if m.Jobs == nil || m.Builds == nil {
		return fmt.Errorf("orka analysis manifest has no jobs or builds map")
	}
	if m.MinToolCalls < 0 {
		return fmt.Errorf("orka analysis manifest min_tool_calls must be non-negative")
	}
	if m.MinGCSBytes < 0 {
		return fmt.Errorf("orka analysis manifest min_gcs_bytes must be non-negative")
	}
	return nil
}
