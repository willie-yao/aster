package orka

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

// AnalysisManifestFile is the private producer-to-ingestor identity contract.
const AnalysisManifestFile = "orka_analysis.json"

const analysisManifestVersion = 7

// AnalysisManifest records the exact Task identity contract for one fetch pass.
type AnalysisManifest struct {
	SchemaVersion         int                      `json:"schema_version"`
	ProjectScope          string                   `json:"project_scope"`
	ProjectLabel          string                   `json:"project_label"`
	ContractHash          string                   `json:"contract_hash"`
	Provider              string                   `json:"provider"`
	Model                 string                   `json:"model"`
	APIMode               string                   `json:"api_mode"`
	Version               string                   `json:"version"`
	MinToolCalls          int                      `json:"min_tool_calls"`
	MinGCSBytes           int                      `json:"min_gcs_bytes"`
	SkillSetHash          string                   `json:"skill_set_hash,omitempty"`
	ValidationKey         string                   `json:"validation_key"`
	ConsecutiveFailures   map[string]int           `json:"consecutive_failures"`
	EvidencePlanHashes    map[string]string        `json:"evidence_plan_hashes"`
	CompleteEvidencePlans map[string]bool          `json:"complete_evidence_plans"`
	Jobs                  map[string]bool          `json:"jobs"`
	Builds                map[string]AnalysisBuild `json:"builds"`
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
func NewAnalysisManifest(projectScope, projectLabel, contractHash, provider, model, apiMode, version string, minToolCalls int) *AnalysisManifest {
	return &AnalysisManifest{
		SchemaVersion:         analysisManifestVersion,
		ProjectScope:          projectScope,
		ProjectLabel:          projectLabel,
		ContractHash:          contractHash,
		Provider:              provider,
		Model:                 model,
		APIMode:               apiMode,
		Version:               version,
		MinToolCalls:          minToolCalls,
		ConsecutiveFailures:   map[string]int{},
		EvidencePlanHashes:    map[string]string{},
		CompleteEvidencePlans: map[string]bool{},
		Jobs:                  map[string]bool{},
		Builds:                map[string]AnalysisBuild{},
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

// SetConsecutiveFailures records the recurrence signal used in the Task prompt.
func (m *AnalysisManifest) SetConsecutiveFailures(jobID, testName string, count int) {
	if count <= 0 {
		return
	}
	if m.ConsecutiveFailures == nil {
		m.ConsecutiveFailures = map[string]int{}
	}
	if count > persistentFailurePromptFloor {
		count = persistentFailurePromptFloor
	}
	m.ConsecutiveFailures[jobID+"::"+testName] = count
}

// SetEvidencePlan records the model-visible evidence-plan identity for one test.
func (m *AnalysisManifest) SetEvidencePlan(jobID, buildID string, testIndex int, plan string, complete bool) {
	if m.EvidencePlanHashes == nil {
		m.EvidencePlanHashes = map[string]string{}
	}
	key := analysisTaskKey(jobID, buildID, testIndex)
	if plan == "" {
		delete(m.EvidencePlanHashes, key)
		return
	}
	m.EvidencePlanHashes[key] = digest(plan, strconv.FormatBool(complete))
}

// SetTaskEvidencePlanComplete records whether a Task can skip recipe lookup.
func (m *AnalysisManifest) SetTaskEvidencePlanComplete(taskName string, complete bool) {
	if m.CompleteEvidencePlans == nil {
		m.CompleteEvidencePlans = map[string]bool{}
	}
	if !complete {
		delete(m.CompleteEvidencePlans, taskName)
		return
	}
	m.CompleteEvidencePlans[taskName] = true
}

// TaskEvidencePlanComplete reports whether the Task received every initial candidate group.
func (m *AnalysisManifest) TaskEvidencePlanComplete(taskName string) bool {
	return m != nil && m.CompleteEvidencePlans[taskName]
}

func analysisTaskKey(jobID, buildID string, testIndex int) string {
	return fmt.Sprintf("%s::%d", BuildKey(jobID, buildID), testIndex)
}

// TaskRef re-derives the exact Task name emitted by the producer.
func (m *AnalysisManifest) TaskRef(jobID string, run models.BuildResult, testIndex int, tc models.TestCase) (AnalysisTaskRef, error) {
	build, ok := m.Builds[BuildKey(jobID, run.BuildID)]
	if !ok {
		return AnalysisTaskRef{}, fmt.Errorf("build identity not found for %s/%s", jobID, run.BuildID)
	}
	consecutiveFailures := m.ConsecutiveFailures[jobID+"::"+tc.Name]
	prompt := FailurePrompt(m.ProjectLabel, jobID, build.Prefix, tc, consecutiveFailures)
	identityPrompt := prompt
	if build.PromptSeedHash != "" {
		identityPrompt += "\nartifact-tree-seed:" + build.PromptSeedHash
	}
	if planHash := m.EvidencePlanHashes[analysisTaskKey(jobID, run.BuildID, testIndex)]; planHash != "" {
		identityPrompt += "\nevidence-plan:" + planHash
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
	if m.ProjectScope == "" || m.ContractHash == "" || m.Provider == "" || m.Model == "" || m.APIMode == "" || m.Version == "" || m.ValidationKey == "" {
		return fmt.Errorf("orka analysis manifest is missing required identity fields")
	}
	if _, err := NormalizeAPIMode(m.APIMode); err != nil {
		return err
	}
	if m.ConsecutiveFailures == nil || m.EvidencePlanHashes == nil || m.CompleteEvidencePlans == nil || m.Jobs == nil || m.Builds == nil {
		return fmt.Errorf("orka analysis manifest has incomplete identity maps")
	}
	for key, count := range m.ConsecutiveFailures {
		if count < 0 {
			return fmt.Errorf("orka analysis manifest consecutive failure count for %s must be non-negative", key)
		}
	}
	if m.MinToolCalls < 0 {
		return fmt.Errorf("orka analysis manifest min_tool_calls must be non-negative")
	}
	if m.MinGCSBytes < 0 {
		return fmt.Errorf("orka analysis manifest min_gcs_bytes must be non-negative")
	}
	return nil
}
