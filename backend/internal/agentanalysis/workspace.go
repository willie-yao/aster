package agentanalysis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/aster/backend/internal/ai"
	"github.com/willie-yao/aster/backend/internal/ai/skills"
	"github.com/willie-yao/aster/backend/internal/artifacts"
	"github.com/willie-yao/aster/backend/internal/modelprovider"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/sourceinvestigation"
)

const (
	WorkspaceManifestVersion = 3
	WorkspaceRequestVersion  = 6
	WorkspaceResultVersion   = 1
	WorkspaceStageVersion    = 4
	WorkspaceContractVersion = "agent-analysis-workspace-v9"
	WorkspaceStageContract   = "agent-analysis-stage-v4"
	WorkspacePromptVersion   = "agent-analysis-workspace-prompt-v9"

	WorkspaceSourceDir            = "source"
	WorkspaceSourcesDir           = "sources"
	WorkspaceArtifactsDir         = "artifacts"
	WorkspaceResultDir            = "result"
	WorkspaceResultFile           = "analysis.json"
	WorkspaceArtifactManifestFile = ".analysis-artifacts.json"
	WorkspaceInputStaged          = "staged"
	WorkspaceInputPrepared        = "prepared"
	WorkspaceStageInputPVC        = "pvc"
	WorkspaceStageInputRemote     = "remote"

	maxWorkspaceFiles        = 5000
	maxWorkspaceFileBytes    = int64(64 << 20)
	maxWorkspaceTotalBytes   = int64(512 << 20)
	maxWorkspacePromptBytes  = 32 << 10
	maxWorkspaceSkillBytes   = 24 << 10
	maxWorkspaceRequestBytes = 768 << 10
	maxWorkspaceStageBytes   = 32 << 10
	WorkspaceMaxSources      = 8
)

var workspaceSourceID = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

// WorkspaceFile identifies one immutable artifact file exposed to OpenCode.
type WorkspaceFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// WorkspaceSourceRef identifies one immutable source checkout.
type WorkspaceSourceRef struct {
	ID         string                         `json:"id"`
	Repository sourceinvestigation.Repository `json:"repository"`
}

// WorkspaceSourceMode binds one source to its prepared filesystem policy.
type WorkspaceSourceMode struct {
	SourceID string                    `json:"source_id"`
	Policy   WorkspaceSourceModePolicy `json:"policy"`
}

// WorkspaceManifest seals one failure to pinned source and artifact snapshots.
type WorkspaceManifest struct {
	Version               int                            `json:"version"`
	ContractVersion       string                         `json:"contract_version"`
	Hash                  string                         `json:"hash"`
	Request               ai.FailureAnalysisRequest      `json:"request"`
	Sources               []WorkspaceSourceRef           `json:"sources"`
	Source                sourceinvestigation.Repository `json:"-"`
	ConsumerPrompt        string                         `json:"consumer_prompt"`
	EffectivePromptSHA256 string                         `json:"effective_prompt_sha256"`
	SkillSetHash          string                         `json:"skill_set_hash"`
	SkillPlan             []skills.PlannedSkill          `json:"skill_plan,omitempty"`
	Artifacts             []WorkspaceFile                `json:"artifacts"`
}

// WorkspaceStageRequest binds staging to sealed source and artifact snapshots.
type WorkspaceStageRequest struct {
	Version                  int                            `json:"version"`
	ContractVersion          string                         `json:"contract_version"`
	Hash                     string                         `json:"hash"`
	ManifestHash             string                         `json:"manifest_hash"`
	BuildPrefix              string                         `json:"build_prefix"`
	Sources                  []WorkspaceSourceRef           `json:"sources"`
	InputMode                string                         `json:"input_mode"`
	ArtifactBaseURL          string                         `json:"artifact_base_url,omitempty"`
	InputSourceModePolicies  []WorkspaceSourceMode          `json:"input_source_mode_policies"`
	OutputSourceModePolicies []WorkspaceSourceMode          `json:"output_source_mode_policies"`
	ArtifactManifestSHA256   string                         `json:"artifact_manifest_sha256"`
	ArtifactFiles            int                            `json:"artifact_files"`
	ArtifactBytes            int64                          `json:"artifact_bytes"`
	Source                   sourceinvestigation.Repository `json:"-"`
	InputSourceModePolicy    WorkspaceSourceModePolicy      `json:"-"`
	OutputSourceModePolicy   WorkspaceSourceModePolicy      `json:"-"`
}

// WorkspaceExecutionRequest is the non-secret request passed to one analyzer Sandbox.
type WorkspaceExecutionRequest struct {
	Version               int                       `json:"version"`
	ContractVersion       string                    `json:"contract_version"`
	Hash                  string                    `json:"hash"`
	PromptVersion         string                    `json:"prompt_version"`
	PromptHash            string                    `json:"prompt_hash"`
	ResultSchemaHash      string                    `json:"result_schema_hash"`
	Manifest              WorkspaceManifest         `json:"manifest"`
	InputMode             string                    `json:"input_mode"`
	SourceModePolicies    []WorkspaceSourceMode     `json:"source_mode_policies"`
	SourceModePolicy      WorkspaceSourceModePolicy `json:"-"`
	RequireSourceEvidence bool                      `json:"require_source_evidence"`
	ModelProvider         modelprovider.Config      `json:"model_provider"`
	TimeoutSeconds        int64                     `json:"timeout_seconds"`
	MaxSteps              int                       `json:"max_steps"`
	ModelContextTokens    int                       `json:"model_context_tokens"`
	ModelOutputTokens     int                       `json:"model_output_tokens"`
	OutputLimitBytes      int64                     `json:"output_limit_bytes"`
}

// UnmarshalJSON restores derived compatibility fields without accepting legacy wire keys.
func (manifest *WorkspaceManifest) UnmarshalJSON(data []byte) error {
	type wire WorkspaceManifest
	var decoded wire
	if err := decodeWorkspaceJSON(data, &decoded); err != nil {
		return err
	}
	*manifest = WorkspaceManifest(decoded)
	if len(manifest.Sources) > 0 {
		manifest.Source = manifest.Sources[0].Repository
	}
	return nil
}

// UnmarshalJSON restores derived compatibility fields without accepting legacy wire keys.
func (stage *WorkspaceStageRequest) UnmarshalJSON(data []byte) error {
	type wire WorkspaceStageRequest
	var decoded wire
	if err := decodeWorkspaceJSON(data, &decoded); err != nil {
		return err
	}
	*stage = WorkspaceStageRequest(decoded)
	if len(stage.Sources) > 0 {
		stage.Source = stage.Sources[0].Repository
	}
	if len(stage.InputSourceModePolicies) > 0 {
		stage.InputSourceModePolicy = stage.InputSourceModePolicies[0].Policy
	}
	if len(stage.OutputSourceModePolicies) > 0 {
		stage.OutputSourceModePolicy = stage.OutputSourceModePolicies[0].Policy
	}
	return nil
}

// UnmarshalJSON restores the derived primary mode policy.
func (request *WorkspaceExecutionRequest) UnmarshalJSON(data []byte) error {
	type wire WorkspaceExecutionRequest
	var decoded wire
	if err := decodeWorkspaceJSON(data, &decoded); err != nil {
		return err
	}
	*request = WorkspaceExecutionRequest(decoded)
	if len(request.SourceModePolicies) > 0 {
		request.SourceModePolicy = request.SourceModePolicies[0].Policy
	}
	return nil
}

func decodeWorkspaceJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("workspace JSON has trailing data")
	}
	return nil
}

// NewWorkspaceManifest creates one deterministic file-backed analyzer input.
func NewWorkspaceManifest(request ai.FailureAnalysisRequest, source sourceinvestigation.Repository, consumerPrompt string, files []WorkspaceFile) (WorkspaceManifest, error) {
	return NewWorkspaceManifestWithSources(request, []WorkspaceSourceRef{{ID: "primary", Repository: source}}, consumerPrompt, files)
}

// NewWorkspaceManifestWithSources creates one deterministic multi-source analyzer input.
func NewWorkspaceManifestWithSources(request ai.FailureAnalysisRequest, sources []WorkspaceSourceRef, consumerPrompt string, files []WorkspaceFile) (WorkspaceManifest, error) {
	return newWorkspaceManifest(request, sources, consumerPrompt, hashString("workspace-skill-input-empty-v1"), nil, files)
}

// EffectivePromptSHA256 returns the canonical in-process prompt identity.
func EffectivePromptSHA256(consumerPrompt string) string {
	consumerPrompt = strings.TrimSpace(strings.ReplaceAll(consumerPrompt, "\r\n", "\n"))
	return hashString(ai.ComposeSystemPrompt(consumerPrompt))
}

func newWorkspaceManifest(request ai.FailureAnalysisRequest, sources []WorkspaceSourceRef, consumerPrompt, skillSetHash string, plan []skills.PlannedSkill, files []WorkspaceFile) (WorkspaceManifest, error) {
	canonicalSources, err := canonicalWorkspaceSources(sources)
	if err != nil {
		return WorkspaceManifest{}, err
	}
	consumerPrompt = strings.TrimSpace(strings.ReplaceAll(consumerPrompt, "\r\n", "\n"))
	manifest := WorkspaceManifest{
		Version: WorkspaceManifestVersion, ContractVersion: WorkspaceContractVersion,
		Request: canonicalRequest(request), Sources: canonicalSources, Source: canonicalSources[0].Repository,
		ConsumerPrompt: consumerPrompt, EffectivePromptSHA256: EffectivePromptSHA256(consumerPrompt),
		SkillSetHash: strings.TrimSpace(skillSetHash), SkillPlan: clonePlan(plan), Artifacts: slices.Clone(files),
	}
	sort.Slice(manifest.Artifacts, func(i, j int) bool { return manifest.Artifacts[i].Path < manifest.Artifacts[j].Path })
	hash, err := workspaceManifestDigest(manifest)
	if err != nil {
		return WorkspaceManifest{}, err
	}
	manifest.Hash = hash
	if err := ValidateWorkspaceManifest(manifest); err != nil {
		return WorkspaceManifest{}, err
	}
	return manifest, nil
}

// ValidateWorkspaceManifest verifies bounds, canonical identity, and file metadata.
func ValidateWorkspaceManifest(manifest WorkspaceManifest) error {
	if manifest.Version != WorkspaceManifestVersion || manifest.ContractVersion != WorkspaceContractVersion {
		return fmt.Errorf("%w: unsupported workspace manifest version", ErrInvalidBundle)
	}
	if err := validateCanonicalWorkspaceSources(manifest.Sources); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidBundle, err)
	}
	if manifest.Source != (sourceinvestigation.Repository{}) && manifest.Source != manifest.Sources[0].Repository {
		return fmt.Errorf("%w: primary source compatibility identity changed", ErrInvalidBundle)
	}
	if manifest.Request.JobID == "" || manifest.Request.BuildPrefix == "" || manifest.Request.Build.BuildID == "" || manifest.Request.TestCase.Name == "" || !requestStringsValid(manifest.Request) {
		return fmt.Errorf("%w: failure request is invalid", ErrInvalidBundle)
	}
	if canonical := canonicalRequest(manifest.Request); !requestsEqual(canonical, manifest.Request) {
		return fmt.Errorf("%w: failure request is not canonical", ErrInvalidBundle)
	}
	if manifest.ConsumerPrompt == "" || manifest.ConsumerPrompt != strings.TrimSpace(manifest.ConsumerPrompt) || !utf8StringWithin(manifest.ConsumerPrompt, maxWorkspacePromptBytes) {
		return fmt.Errorf("%w: consumer prompt is empty, invalid, or oversized", ErrInvalidBundle)
	}
	if !validSHA256(manifest.EffectivePromptSHA256) || manifest.EffectivePromptSHA256 != EffectivePromptSHA256(manifest.ConsumerPrompt) {
		return fmt.Errorf("%w: effective prompt identity is invalid", ErrInvalidBundle)
	}
	if !validSHA256(manifest.SkillSetHash) {
		return fmt.Errorf("%w: skill set identity is invalid", ErrInvalidBundle)
	}
	planData, err := json.Marshal(manifest.SkillPlan)
	if err != nil || len(planData) > maxWorkspaceSkillBytes {
		return fmt.Errorf("%w: workspace skill plan is invalid or oversized", ErrInvalidBundle)
	}
	if err := validateWorkspaceFiles(manifest.Artifacts); err != nil {
		return err
	}
	if !validSHA256(manifest.Hash) {
		return fmt.Errorf("%w: workspace manifest hash is invalid", ErrInvalidBundle)
	}
	digest, err := workspaceManifestDigest(manifest)
	if err != nil || digest != manifest.Hash {
		return fmt.Errorf("%w: workspace manifest hash changed", ErrInvalidBundle)
	}
	return nil
}

// NewWorkspaceStageRequest creates the default mode-preserving stager input.
func NewWorkspaceStageRequest(manifest WorkspaceManifest) (WorkspaceStageRequest, error) {
	return NewWorkspaceStageRequestWithSourceModePolicies(manifest, WorkspaceSourceModePreserve, WorkspaceSourceModePreserve)
}

// NewWorkspaceStageRequestWithSourceModePolicies seals one policy for every source.
func NewWorkspaceStageRequestWithSourceModePolicies(manifest WorkspaceManifest, inputPolicy, outputPolicy WorkspaceSourceModePolicy) (WorkspaceStageRequest, error) {
	return newWorkspaceStageRequest(manifest, WorkspaceStageInputPVC, "", workspaceSourceModes(manifest.Sources, inputPolicy), workspaceSourceModes(manifest.Sources, outputPolicy))
}

// NewWorkspaceStageRequestWithPolicies seals exact per-source filesystem mode policies.
func NewWorkspaceStageRequestWithPolicies(manifest WorkspaceManifest, inputPolicies, outputPolicies []WorkspaceSourceMode) (WorkspaceStageRequest, error) {
	return newWorkspaceStageRequest(manifest, WorkspaceStageInputPVC, "", inputPolicies, outputPolicies)
}

// NewWorkspaceRemoteStageRequest creates a credential-free HTTPS staging request.
func NewWorkspaceRemoteStageRequest(manifest WorkspaceManifest, artifactBaseURL string, localInputPolicy WorkspaceSourceModePolicy) (WorkspaceStageRequest, error) {
	return newWorkspaceStageRequest(manifest, WorkspaceStageInputRemote, strings.TrimRight(strings.TrimSpace(artifactBaseURL), "/"), workspaceSourceModes(manifest.Sources, localInputPolicy), workspaceSourceModes(manifest.Sources, WorkspaceSourceModePreserve))
}

func newWorkspaceStageRequest(manifest WorkspaceManifest, inputMode, artifactBaseURL string, inputPolicies, outputPolicies []WorkspaceSourceMode) (WorkspaceStageRequest, error) {
	if err := ValidateWorkspaceManifest(manifest); err != nil {
		return WorkspaceStageRequest{}, err
	}
	stage := WorkspaceStageRequest{
		Version: WorkspaceStageVersion, ContractVersion: WorkspaceStageContract,
		ManifestHash: manifest.Hash, BuildPrefix: manifest.Request.BuildPrefix,
		Sources: slices.Clone(manifest.Sources), Source: manifest.Sources[0].Repository,
		InputMode: inputMode, ArtifactBaseURL: artifactBaseURL,
		InputSourceModePolicies: slices.Clone(inputPolicies), OutputSourceModePolicies: slices.Clone(outputPolicies),
	}
	if len(stage.InputSourceModePolicies) > 0 {
		stage.InputSourceModePolicy = stage.InputSourceModePolicies[0].Policy
	}
	if len(stage.OutputSourceModePolicies) > 0 {
		stage.OutputSourceModePolicy = stage.OutputSourceModePolicies[0].Policy
	}
	var err error
	stage.ArtifactManifestSHA256, stage.ArtifactFiles, stage.ArtifactBytes, err = WorkspaceArtifactIdentity(manifest.Artifacts)
	if err != nil {
		return WorkspaceStageRequest{}, err
	}
	hash, err := workspaceStageDigest(stage)
	if err != nil {
		return WorkspaceStageRequest{}, err
	}
	stage.Hash = hash
	if err := ValidateWorkspaceStageRequest(stage, manifest); err != nil {
		return WorkspaceStageRequest{}, err
	}
	return stage, nil
}

// ValidateWorkspaceStageRequestIdentity validates one self-contained stager request.
func ValidateWorkspaceStageRequestIdentity(stage WorkspaceStageRequest) error {
	if stage.Version != WorkspaceStageVersion || stage.ContractVersion != WorkspaceStageContract || !validSHA256(stage.ManifestHash) || strings.TrimSpace(stage.BuildPrefix) == "" || stage.BuildPrefix != strings.TrimSpace(stage.BuildPrefix) || strings.IndexByte(stage.BuildPrefix, 0) >= 0 || len(stage.BuildPrefix) > maxFailureStringBytes {
		return fmt.Errorf("workspace stage request identity is invalid")
	}
	if err := validateCanonicalWorkspaceSources(stage.Sources); err != nil {
		return fmt.Errorf("workspace stage source identity is invalid: %w", err)
	}
	if stage.Source != (sourceinvestigation.Repository{}) && stage.Source != stage.Sources[0].Repository {
		return fmt.Errorf("workspace stage primary source compatibility identity changed")
	}
	switch stage.InputMode {
	case WorkspaceStageInputPVC:
		if stage.ArtifactBaseURL != "" {
			return fmt.Errorf("workspace PVC stage must not set an artifact URL")
		}
	case WorkspaceStageInputRemote:
		if err := validateWorkspaceArtifactBaseURL(stage.ArtifactBaseURL); err != nil {
			return err
		}
	default:
		return fmt.Errorf("workspace stage input mode is invalid")
	}
	if err := validateWorkspaceSourceModes(stage.Sources, stage.InputSourceModePolicies); err != nil {
		return fmt.Errorf("workspace stage input source mode policies are invalid: %w", err)
	}
	if err := validateWorkspaceSourceModes(stage.Sources, stage.OutputSourceModePolicies); err != nil {
		return fmt.Errorf("workspace stage output source mode policies are invalid: %w", err)
	}
	if stage.InputSourceModePolicy != "" && stage.InputSourceModePolicy != stage.InputSourceModePolicies[0].Policy {
		return fmt.Errorf("workspace stage primary input source mode compatibility policy changed")
	}
	if stage.OutputSourceModePolicy != "" && stage.OutputSourceModePolicy != stage.OutputSourceModePolicies[0].Policy {
		return fmt.Errorf("workspace stage primary output source mode compatibility policy changed")
	}
	if !validSHA256(stage.ArtifactManifestSHA256) || stage.ArtifactFiles < 1 || stage.ArtifactFiles > maxWorkspaceFiles || stage.ArtifactBytes < 0 || stage.ArtifactBytes > maxWorkspaceTotalBytes {
		return fmt.Errorf("workspace stage artifact identity is invalid")
	}
	if !validSHA256(stage.Hash) {
		return fmt.Errorf("workspace stage request hash is invalid")
	}
	digest, err := workspaceStageDigest(stage)
	if err != nil || digest != stage.Hash {
		return fmt.Errorf("workspace stage request hash changed")
	}
	data, err := json.Marshal(stage)
	if err != nil || len(data) > maxWorkspaceStageBytes {
		return fmt.Errorf("workspace stage request exceeds %d bytes", maxWorkspaceStageBytes)
	}
	return nil
}

// ValidateWorkspaceStageRequest requires exact manifest and artifact identity.
func ValidateWorkspaceStageRequest(stage WorkspaceStageRequest, manifest WorkspaceManifest) error {
	if err := ValidateWorkspaceStageRequestIdentity(stage); err != nil {
		return err
	}
	if err := ValidateWorkspaceManifest(manifest); err != nil {
		return err
	}
	artifactHash, artifactFiles, artifactBytes, err := WorkspaceArtifactIdentity(manifest.Artifacts)
	if err != nil {
		return err
	}
	if stage.ManifestHash != manifest.Hash || stage.BuildPrefix != manifest.Request.BuildPrefix || !slices.Equal(stage.Sources, manifest.Sources) || stage.ArtifactManifestSHA256 != artifactHash || stage.ArtifactFiles != artifactFiles || stage.ArtifactBytes != artifactBytes {
		return fmt.Errorf("workspace stage request does not match the sealed manifest")
	}
	return nil
}

// WorkspaceSource returns one source by stable ID.
func WorkspaceSource(sources []WorkspaceSourceRef, id string) (WorkspaceSourceRef, bool) {
	index, found := slices.BinarySearchFunc(sources, strings.TrimSpace(id), func(source WorkspaceSourceRef, target string) int {
		return strings.Compare(source.ID, target)
	})
	if !found {
		return WorkspaceSourceRef{}, false
	}
	return sources[index], true
}

// WorkspaceSourceModeFor returns one source policy by stable ID.
func WorkspaceSourceModeFor(policies []WorkspaceSourceMode, id string) (WorkspaceSourceModePolicy, bool) {
	index, found := slices.BinarySearchFunc(policies, strings.TrimSpace(id), func(mode WorkspaceSourceMode, target string) int {
		return strings.Compare(mode.SourceID, target)
	})
	if !found {
		return "", false
	}
	return policies[index].Policy, true
}

func canonicalWorkspaceSources(sources []WorkspaceSourceRef) ([]WorkspaceSourceRef, error) {
	canonical := slices.Clone(sources)
	for index := range canonical {
		canonical[index].ID = strings.TrimSpace(canonical[index].ID)
		canonical[index].Repository.Owner = strings.TrimSpace(canonical[index].Repository.Owner)
		canonical[index].Repository.Name = strings.TrimSpace(canonical[index].Repository.Name)
		canonical[index].Repository.Revision = strings.TrimSpace(canonical[index].Repository.Revision)
	}
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].ID < canonical[j].ID })
	if err := validateCanonicalWorkspaceSources(canonical); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
	}
	return canonical, nil
}

func validateCanonicalWorkspaceSources(sources []WorkspaceSourceRef) error {
	if len(sources) < 1 || len(sources) > WorkspaceMaxSources {
		return fmt.Errorf("workspace sources must contain between 1 and %d entries", WorkspaceMaxSources)
	}
	ids := make(map[string]struct{}, len(sources))
	identities := make(map[string]struct{}, len(sources))
	previous := ""
	for _, source := range sources {
		if !workspaceSourceID.MatchString(source.ID) || source.ID <= previous {
			return fmt.Errorf("workspace source IDs are invalid or not canonical")
		}
		if err := sourceinvestigation.ValidateRepository(source.Repository); err != nil || !immutableSourceRevision.MatchString(source.Repository.Revision) {
			return fmt.Errorf("workspace source repository is invalid")
		}
		if source.Repository.Owner != strings.TrimSpace(source.Repository.Owner) || source.Repository.Name != strings.TrimSpace(source.Repository.Name) || source.Repository.Revision != strings.TrimSpace(source.Repository.Revision) {
			return fmt.Errorf("workspace source repository is not canonical")
		}
		identity := source.Repository.Owner + "/" + source.Repository.Name + "@" + source.Repository.Revision
		if _, ok := ids[source.ID]; ok {
			return fmt.Errorf("workspace source ID is duplicated")
		}
		if _, ok := identities[identity]; ok {
			return fmt.Errorf("workspace source identity is duplicated")
		}
		ids[source.ID] = struct{}{}
		identities[identity] = struct{}{}
		previous = source.ID
	}
	return nil
}

func workspaceSourceModes(sources []WorkspaceSourceRef, policy WorkspaceSourceModePolicy) []WorkspaceSourceMode {
	modes := make([]WorkspaceSourceMode, 0, len(sources))
	for _, source := range sources {
		modes = append(modes, WorkspaceSourceMode{SourceID: source.ID, Policy: policy})
	}
	return modes
}

func validateWorkspaceSourceModes(sources []WorkspaceSourceRef, policies []WorkspaceSourceMode) error {
	if len(policies) != len(sources) {
		return fmt.Errorf("source policy count does not match source count")
	}
	for index, policy := range policies {
		if policy.SourceID != sources[index].ID || !validWorkspaceSourceModePolicy(policy.Policy) {
			return fmt.Errorf("source policies are invalid or not canonical")
		}
	}
	return nil
}

// SnapshotArtifactWorkspace records every regular file under a bounded root.
func SnapshotArtifactWorkspace(root string) ([]WorkspaceFile, error) {
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("artifact workspace root is not a directory")
	}
	files := make([]WorkspaceFile, 0)
	var total int64
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact workspace contains symlink %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifact workspace contains non-regular file %s", path)
		}
		if info.Size() > maxWorkspaceFileBytes {
			return fmt.Errorf("artifact file %s exceeds %d bytes", path, maxWorkspaceFileBytes)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		clean, err := artifacts.SafePath(relative)
		if err != nil || clean != relative {
			return fmt.Errorf("artifact workspace path %s is unsafe", relative)
		}
		hash, size, err := hashWorkspaceFile(path)
		if err != nil {
			return err
		}
		if size != info.Size() {
			return fmt.Errorf("artifact file %s changed while hashing", relative)
		}
		total += size
		if total > maxWorkspaceTotalBytes {
			return fmt.Errorf("artifact workspace exceeds %d bytes", maxWorkspaceTotalBytes)
		}
		files = append(files, WorkspaceFile{Path: relative, Size: size, SHA256: hash})
		if len(files) > maxWorkspaceFiles {
			return fmt.Errorf("artifact workspace exceeds %d files", maxWorkspaceFiles)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("artifact workspace is empty")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// VerifyArtifactFiles confirms a directory matches exact artifact identities.
func VerifyArtifactFiles(root string, expected []WorkspaceFile) error {
	if err := validateWorkspaceFiles(expected); err != nil {
		return err
	}
	files, err := SnapshotArtifactWorkspace(root)
	if err != nil {
		return err
	}
	left, _ := json.Marshal(files)
	right, _ := json.Marshal(expected)
	if !bytes.Equal(left, right) {
		return fmt.Errorf("artifact workspace does not match the sealed manifest")
	}
	return nil
}

// VerifyArtifactWorkspace confirms the mounted files match the sealed manifest.
func VerifyArtifactWorkspace(root string, manifest WorkspaceManifest) error {
	if err := ValidateWorkspaceManifest(manifest); err != nil {
		return err
	}
	return VerifyArtifactFiles(root, manifest.Artifacts)
}

// VerifySourceWorkspace confirms a mode-preserving checkout exactly matches the pinned commit.
func VerifySourceWorkspace(ctx context.Context, root, revision string) error {
	return verifySourceWorkspace(ctx, root, revision, WorkspaceSourceModePreserve)
}

// VerifyPreparedSourceWorkspace confirms a prepared checkout matches its sealed filesystem mode policy.
func VerifyPreparedSourceWorkspace(ctx context.Context, root, revision string, modePolicy WorkspaceSourceModePolicy) error {
	return verifySourceWorkspace(ctx, root, revision, modePolicy)
}

func verifySourceWorkspace(ctx context.Context, root, revision string, modePolicy WorkspaceSourceModePolicy) error {
	if !immutableSourceRevision.MatchString(revision) {
		return fmt.Errorf("source revision is invalid")
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source workspace root is not a safe directory")
	}
	if !validWorkspaceSourceModePolicy(modePolicy) {
		return fmt.Errorf("source workspace mode policy is invalid")
	}
	if err := validateSourceGitMetadata(root); err != nil {
		return err
	}
	configuredMode, err := gitWorkspaceOutput(ctx, root, "config", "--local", "--bool", "--get", "core.filemode")
	if err != nil {
		return fmt.Errorf("inspect source workspace mode policy: %w", err)
	}
	wantFileMode := "true"
	if modePolicy == WorkspaceSourceModeIgnoreExecutable {
		wantFileMode = "false"
	}
	if strings.TrimSpace(string(configuredMode)) != wantFileMode {
		return sourceIntegrityFailure(SourceModePolicyChanged, nil)
	}
	head, err := gitWorkspaceOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(head)) != revision {
		return fmt.Errorf("source workspace does not match the pinned revision")
	}
	tracked, err := gitWorkspaceOutput(ctx, root, "ls-files", "-v", "-z")
	if err != nil {
		return fmt.Errorf("inspect source index flags: %w", err)
	}
	for _, record := range bytes.Split(tracked, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		if len(record) < 3 || record[0] != 'H' || record[1] != ' ' {
			return sourceIntegrityFailure(SourceIndexFlagsChanged, nil)
		}
	}
	staged, err := gitWorkspaceOutput(ctx, root, "ls-files", "--stage", "-z")
	if err != nil {
		return fmt.Errorf("inspect source index modes: %w", err)
	}
	var directorySymlinks []sourceDirectorySymlink
	for _, record := range bytes.Split(staged, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		mode, path, err := sourceIndexEntry(record)
		if err != nil {
			return err
		}
		switch mode {
		case "100644", "100755":
		case "120000":
			target, info, exists, err := validateSourceSymlink(root, path)
			if err != nil {
				return err
			}
			if exists && info.IsDir() {
				directorySymlinks = append(directorySymlinks, sourceDirectorySymlink{Path: path, Target: target})
			}
		default:
			return fmt.Errorf("source workspace contains an unsupported index mode %s", mode)
		}
	}
	if err := validateSourceDirectoryGraph(root, directorySymlinks); err != nil {
		return err
	}
	stagedExit, err := sourceDiffExitCode(ctx, root, "diff", "--cached", "--no-ext-diff", "--no-textconv", "--quiet", revision, "--")
	if err != nil {
		return err
	}
	if stagedExit == 1 {
		contentChanges, modeChanges, err := stagedDiffCounts(ctx, root, revision)
		if err != nil {
			return err
		}
		if contentChanges > 0 {
			return sourceIntegrityFailure(SourceStagedContentChanged, nil)
		}
		if modeChanges > 0 {
			return sourceIntegrityFailure(SourceIndexModeChanged, nil)
		}
		return sourceIntegrityFailure(SourceStagedContentChanged, nil)
	}
	contentExit, err := sourceDiffExitCode(ctx, root, "-c", "core.filemode=false", "diff", "--no-ext-diff", "--no-textconv", "--quiet", "--")
	if err != nil {
		return err
	}
	if contentExit == 1 {
		return sourceIntegrityFailure(SourceWorktreeContentChanged, nil)
	}
	if modePolicy == WorkspaceSourceModePreserve {
		worktreeExit, err := sourceDiffExitCode(ctx, root, "diff", "--no-ext-diff", "--no-textconv", "--quiet", "--")
		if err != nil {
			return err
		}
		if worktreeExit == 1 {
			return sourceIntegrityFailure(SourceWorktreeModeChanged, nil)
		}
	}
	for _, args := range [][]string{{"ls-files", "--others", "--exclude-standard", "-z"}, {"ls-files", "--others", "--ignored", "--exclude-standard", "-z"}} {
		output, err := gitWorkspaceOutput(ctx, root, args...)
		if err != nil {
			return fmt.Errorf("inspect source workspace extras: %w", err)
		}
		if len(output) != 0 {
			return sourceIntegrityFailure(SourceUntrackedFiles, nil)
		}
	}
	return nil
}

func validateSourceGitMetadata(root string) error {
	gitRoot := filepath.Join(root, ".git")
	info, err := os.Lstat(gitRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source workspace Git metadata is not a safe directory")
	}
	return filepath.WalkDir(gitRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != gitRoot && entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("source workspace Git metadata contains a symlink")
		}
		return nil
	})
}

func workspaceGitEnvironment() []string {
	env := []string{}
	for _, name := range []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS"} {
		if value, ok := os.LookupEnv(name); ok && value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func gitWorkspaceOutput(ctx context.Context, root string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	command.Env = append(workspaceGitEnvironment(), "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1")
	return command.CombinedOutput()
}

// NewWorkspaceExecutionRequest seals default mode-preserving runtime bounds and gateway identity.
func NewWorkspaceExecutionRequest(manifest WorkspaceManifest, provider modelprovider.Config, timeout time.Duration, maxSteps, modelContextTokens, modelOutputTokens int, outputLimit int64) (WorkspaceExecutionRequest, error) {
	return NewWorkspaceExecutionRequestWithSourceModePolicy(manifest, WorkspaceSourceModePreserve, provider, timeout, maxSteps, modelContextTokens, modelOutputTokens, outputLimit)
}

// NewWorkspaceExecutionRequestWithSourceModePolicy seals the prepared filesystem mode policy.
func NewWorkspaceExecutionRequestWithSourceModePolicy(manifest WorkspaceManifest, modePolicy WorkspaceSourceModePolicy, provider modelprovider.Config, timeout time.Duration, maxSteps, modelContextTokens, modelOutputTokens int, outputLimit int64) (WorkspaceExecutionRequest, error) {
	return NewWorkspaceExecutionRequestWithSourceEvidence(manifest, modePolicy, false, provider, timeout, maxSteps, modelContextTokens, modelOutputTokens, outputLimit)
}

// NewWorkspaceExecutionRequestWithSourceEvidence seals an optional source-evidence floor.
func NewWorkspaceExecutionRequestWithSourceEvidence(manifest WorkspaceManifest, modePolicy WorkspaceSourceModePolicy, requireSourceEvidence bool, provider modelprovider.Config, timeout time.Duration, maxSteps, modelContextTokens, modelOutputTokens int, outputLimit int64) (WorkspaceExecutionRequest, error) {
	return NewWorkspaceExecutionRequestWithSourcePolicies(manifest, workspaceSourceModes(manifest.Sources, modePolicy), requireSourceEvidence, provider, timeout, maxSteps, modelContextTokens, modelOutputTokens, outputLimit)
}

// NewWorkspaceExecutionRequestWithSourcePolicies seals exact per-source policies.
func NewWorkspaceExecutionRequestWithSourcePolicies(manifest WorkspaceManifest, policies []WorkspaceSourceMode, requireSourceEvidence bool, provider modelprovider.Config, timeout time.Duration, maxSteps, modelContextTokens, modelOutputTokens int, outputLimit int64) (WorkspaceExecutionRequest, error) {
	request := WorkspaceExecutionRequest{
		Version: WorkspaceRequestVersion, ContractVersion: WorkspaceContractVersion, PromptVersion: WorkspacePromptVersion,
		PromptHash: WorkspaceSkillHash(), ResultSchemaHash: WorkspaceResultSchemaHash(), Manifest: manifest, InputMode: WorkspaceInputStaged, SourceModePolicies: slices.Clone(policies), RequireSourceEvidence: requireSourceEvidence, ModelProvider: provider, TimeoutSeconds: int64(timeout.Round(time.Second) / time.Second),
		MaxSteps: maxSteps, ModelContextTokens: modelContextTokens, ModelOutputTokens: modelOutputTokens, OutputLimitBytes: outputLimit,
	}
	if len(request.SourceModePolicies) > 0 {
		request.SourceModePolicy = request.SourceModePolicies[0].Policy
	}
	hash, err := workspaceRequestDigest(request)
	if err != nil {
		return WorkspaceExecutionRequest{}, err
	}
	request.Hash = hash
	if err := ValidateWorkspaceExecutionRequest(request); err != nil {
		return WorkspaceExecutionRequest{}, err
	}
	return request, nil
}

// ValidateWorkspaceExecutionRequest validates one non-secret analyzer request.
func ValidateWorkspaceExecutionRequest(request WorkspaceExecutionRequest) error {
	if request.Version != WorkspaceRequestVersion || request.ContractVersion != WorkspaceContractVersion || request.PromptVersion != WorkspacePromptVersion || request.PromptHash != WorkspaceSkillHash() || request.ResultSchemaHash != WorkspaceResultSchemaHash() {
		return fmt.Errorf("workspace analysis request version is invalid")
	}
	if err := ValidateWorkspaceManifest(request.Manifest); err != nil {
		return err
	}
	if request.InputMode != WorkspaceInputStaged && request.InputMode != WorkspaceInputPrepared {
		return fmt.Errorf("workspace analysis input mode is invalid")
	}
	if err := validateWorkspaceSourceModes(request.Manifest.Sources, request.SourceModePolicies); err != nil {
		return fmt.Errorf("workspace analysis source mode policies are invalid: %w", err)
	}
	if request.SourceModePolicy != "" && request.SourceModePolicy != request.SourceModePolicies[0].Policy {
		return fmt.Errorf("workspace analysis primary source mode compatibility policy changed")
	}
	if err := modelprovider.ValidateDeploymentEndpoint(request.ModelProvider); err != nil {
		return fmt.Errorf("workspace analysis model provider: %w", err)
	}
	if _, err := modelprovider.OpenCodeBaseURL(request.ModelProvider); err != nil {
		return fmt.Errorf("workspace analysis model provider: %w", err)
	}
	if request.ModelProvider.CredentialMode == modelprovider.CredentialModeGateway {
		if err := engineruntime.ValidateModelGatewayTrust(request.ModelProvider.Endpoint, request.ModelProvider.PublicCAPrivateDNS); err != nil {
			return fmt.Errorf("workspace analysis model provider gateway: %w", err)
		}
	}
	if request.TimeoutSeconds < 1 || request.TimeoutSeconds > int64((30*time.Minute)/time.Second) {
		return fmt.Errorf("workspace analysis timeout must be between 1 second and 30 minutes")
	}
	if request.MaxSteps < 3 || request.MaxSteps > 100 {
		return fmt.Errorf("workspace analysis max steps must be between 3 and 100")
	}
	if request.RequireSourceEvidence && request.MaxSteps < 5 {
		return fmt.Errorf("workspace analysis with required source evidence needs at least 5 steps")
	}
	if request.ModelContextTokens < 8192 || request.ModelContextTokens > 2_000_000 || request.ModelOutputTokens < 1024 || request.ModelOutputTokens > request.ModelContextTokens || request.ModelOutputTokens > 131072 {
		return fmt.Errorf("workspace analysis model limits are invalid")
	}
	if request.OutputLimitBytes < 4<<10 || request.OutputLimitBytes > 1<<20 {
		return fmt.Errorf("workspace analysis output limit must be between 4096 and 1048576 bytes")
	}
	if !validSHA256(request.Hash) {
		return fmt.Errorf("workspace analysis request hash is invalid")
	}
	digest, err := workspaceRequestDigest(request)
	if err != nil || digest != request.Hash {
		return fmt.Errorf("workspace analysis request hash changed")
	}
	data, err := json.Marshal(request)
	if err != nil || len(data) > maxWorkspaceRequestBytes {
		return fmt.Errorf("workspace analysis request exceeds %d bytes", maxWorkspaceRequestBytes)
	}
	return nil
}

func sourceIndexEntry(record []byte) (string, string, error) {
	tab := bytes.IndexByte(record, '\t')
	if tab < 0 || tab == len(record)-1 {
		return "", "", fmt.Errorf("source workspace index entry is malformed")
	}
	fields := bytes.Fields(record[:tab])
	if len(fields) != 3 || len(fields[0]) != 6 {
		return "", "", fmt.Errorf("source workspace index entry is malformed")
	}
	path := filepath.Clean(filepath.FromSlash(string(record[tab+1:])))
	if filepath.IsAbs(path) || path == "." || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("source workspace index path is unsafe")
	}
	return string(fields[0]), path, nil
}

func validateSourceSymlink(root, path string) (string, os.FileInfo, bool, error) {
	info, err := os.Lstat(filepath.Join(root, path))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", nil, false, fmt.Errorf("source workspace symlink %s is not materialized safely", filepath.ToSlash(path))
	}
	target, targetInfo, exists, err := resolveSourcePathWithinRoot(root, path, map[string]bool{}, 0)
	if err != nil {
		return "", nil, false, fmt.Errorf("source workspace symlink %s is unsafe: %w", filepath.ToSlash(path), err)
	}
	if target == ".git" || strings.HasPrefix(target, ".git"+string(filepath.Separator)) {
		return "", nil, false, fmt.Errorf("source workspace symlink %s targets Git metadata", filepath.ToSlash(path))
	}
	return target, targetInfo, exists, nil
}

func resolveSourcePathWithinRoot(root, relative string, seen map[string]bool, depth int) (string, os.FileInfo, bool, error) {
	if depth > 64 {
		return "", nil, false, fmt.Errorf("symlink chain is too deep")
	}
	relative = filepath.Clean(relative)
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", nil, false, fmt.Errorf("target escapes the source root")
	}
	if relative == "." {
		info, err := os.Lstat(root)
		return relative, info, err == nil, err
	}
	parts := strings.Split(relative, string(filepath.Separator))
	resolved := ""
	var finalInfo os.FileInfo
	for index, part := range parts {
		if part == "" || part == "." {
			continue
		}
		resolved = filepath.Join(resolved, part)
		info, err := os.Lstat(filepath.Join(root, resolved))
		if os.IsNotExist(err) {
			return relative, nil, false, nil
		}
		if err != nil {
			return "", nil, false, err
		}
		finalInfo = info
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if seen[resolved] {
			return "", nil, false, fmt.Errorf("symlink chain contains a cycle")
		}
		seen[resolved] = true
		target, err := os.Readlink(filepath.Join(root, resolved))
		if err != nil {
			return "", nil, false, err
		}
		if target == "" || filepath.IsAbs(target) || strings.IndexByte(target, 0) >= 0 {
			return "", nil, false, fmt.Errorf("target must be a non-empty relative path")
		}
		remaining := filepath.Join(parts[index+1:]...)
		next := filepath.Join(filepath.Dir(resolved), filepath.FromSlash(target), remaining)
		return resolveSourcePathWithinRoot(root, next, seen, depth+1)
	}
	return relative, finalInfo, finalInfo != nil, nil
}

type sourceDirectorySymlink struct {
	Path   string
	Target string
}

func validateSourceDirectoryGraph(root string, symlinks []sourceDirectorySymlink) error {
	graph := map[string][]string{".": {}}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == ".git" {
			return filepath.SkipDir
		}
		if path == root || !entry.IsDir() {
			return nil
		}
		parent := filepath.Dir(relative)
		graph[parent] = append(graph[parent], relative)
		if _, ok := graph[relative]; !ok {
			graph[relative] = nil
		}
		return nil
	}); err != nil {
		return fmt.Errorf("inspect source directory graph: %w", err)
	}
	for _, symlink := range symlinks {
		parent := filepath.Dir(symlink.Path)
		graph[parent] = append(graph[parent], symlink.Target)
		if _, ok := graph[symlink.Target]; !ok {
			graph[symlink.Target] = nil
		}
	}
	state := make(map[string]uint8, len(graph))
	var visit func(string) error
	visit = func(node string) error {
		switch state[node] {
		case 1:
			return fmt.Errorf("source workspace directory symlinks contain a cycle")
		case 2:
			return nil
		}
		state[node] = 1
		for _, next := range graph[node] {
			if err := visit(next); err != nil {
				return err
			}
		}
		state[node] = 2
		return nil
	}
	for node := range graph {
		if err := visit(node); err != nil {
			return err
		}
	}
	return nil
}

func validateWorkspaceFiles(files []WorkspaceFile) error {
	if len(files) < 1 || len(files) > maxWorkspaceFiles {
		return fmt.Errorf("%w: artifact count must be between 1 and %d", ErrInvalidBundle, maxWorkspaceFiles)
	}
	var total int64
	previous := ""
	for index, file := range files {
		clean, err := artifacts.SafePath(file.Path)
		if err != nil || clean != file.Path || file.Path <= previous {
			return fmt.Errorf("%w: artifact %d path is unsafe, duplicated, or unsorted", ErrInvalidBundle, index)
		}
		if file.Size < 0 || file.Size > maxWorkspaceFileBytes || !validSHA256(file.SHA256) {
			return fmt.Errorf("%w: artifact %d metadata is invalid", ErrInvalidBundle, index)
		}
		total += file.Size
		if total > maxWorkspaceTotalBytes {
			return fmt.Errorf("%w: artifact bytes exceed %d", ErrInvalidBundle, maxWorkspaceTotalBytes)
		}
		previous = file.Path
	}
	return nil
}

func workspaceManifestDigest(manifest WorkspaceManifest) (string, error) {
	manifest.Hash = ""
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return hashString(string(data)), nil
}

func workspaceStageDigest(stage WorkspaceStageRequest) (string, error) {
	stage.Hash = ""
	data, err := json.Marshal(stage)
	if err != nil {
		return "", err
	}
	return hashString(string(data)), nil
}

func workspaceRequestDigest(request WorkspaceExecutionRequest) (string, error) {
	request.Hash = ""
	data, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	return hashString(string(data)), nil
}

type workspaceArtifactManifest struct {
	Version   int             `json:"version"`
	Artifacts []WorkspaceFile `json:"artifacts"`
}

// WorkspaceArtifactIdentity returns the canonical artifact-index identity.
func WorkspaceArtifactIdentity(files []WorkspaceFile) (string, int, int64, error) {
	if err := validateWorkspaceFiles(files); err != nil {
		return "", 0, 0, err
	}
	data, err := json.Marshal(workspaceArtifactManifest{Version: 1, Artifacts: files})
	if err != nil {
		return "", 0, 0, err
	}
	var total int64
	for _, file := range files {
		total += file.Size
	}
	return hashString(string(data)), len(files), total, nil
}

// WriteWorkspaceArtifactManifest writes the canonical private artifact index.
func WriteWorkspaceArtifactManifest(snapshotRoot string, files []WorkspaceFile) error {
	if _, _, _, err := WorkspaceArtifactIdentity(files); err != nil {
		return err
	}
	data, err := json.Marshal(workspaceArtifactManifest{Version: 1, Artifacts: files})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(filepath.Clean(snapshotRoot), WorkspaceArtifactManifestFile), append(data, '\n'), 0o600)
}

// ReadWorkspaceArtifactManifest verifies and returns one staged artifact index.
func ReadWorkspaceArtifactManifest(snapshotRoot string, stage WorkspaceStageRequest) ([]WorkspaceFile, error) {
	data, err := os.ReadFile(filepath.Join(filepath.Clean(snapshotRoot), WorkspaceArtifactManifestFile))
	if err != nil || len(data) > 1<<20 {
		return nil, fmt.Errorf("workspace artifact manifest is unavailable")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest workspaceArtifactManifest
	if err := decoder.Decode(&manifest); err != nil || manifest.Version != 1 {
		return nil, fmt.Errorf("workspace artifact manifest is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("workspace artifact manifest has trailing data")
	}
	hash, count, total, err := WorkspaceArtifactIdentity(manifest.Artifacts)
	if err != nil || hash != stage.ArtifactManifestSHA256 || count != stage.ArtifactFiles || total != stage.ArtifactBytes {
		return nil, fmt.Errorf("workspace artifact manifest identity changed")
	}
	return manifest.Artifacts, nil
}

func validateWorkspaceArtifactBaseURL(value string) error {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.HasSuffix(parsed.Path, "/") {
		return fmt.Errorf("workspace artifact base URL is invalid")
	}
	return nil
}

func hashWorkspaceFile(path string) (string, int64, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, maxWorkspaceFileBytes+1))
	if err != nil {
		return "", 0, err
	}
	if size > maxWorkspaceFileBytes {
		return "", 0, fmt.Errorf("artifact file exceeds %d bytes", maxWorkspaceFileBytes)
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func utf8StringWithin(value string, limit int) bool {
	return strings.IndexByte(value, 0) < 0 && len(value) <= limit && strings.ToValidUTF8(value, "") == value
}
