package agentanalysis

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	WorkspacePublishVersion  = 1
	WorkspacePublishContract = "agent-analysis-publish-v1"
	WorkspaceCleanupVersion  = 1
	WorkspaceCleanupContract = "agent-analysis-input-cleanup-v1"
	// WorkspacePublishEncodedMaxBytes is the admitted Base64 environment-value bound.
	WorkspacePublishEncodedMaxBytes = 1 << 20
	// WorkspacePublishRawMaxBytes is the largest raw JSON whose Base64 form fits.
	WorkspacePublishRawMaxBytes = 3 * (WorkspacePublishEncodedMaxBytes / 4)
)

var workspaceLeaseIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,126}[a-z0-9])?$`)

// WorkspacePublishRequest copies one remotely retrievable snapshot into private input storage.
type WorkspacePublishRequest struct {
	Version         int                   `json:"version"`
	ContractVersion string                `json:"contract_version"`
	Hash            string                `json:"hash"`
	LeaseID         string                `json:"lease_id"`
	Stage           WorkspaceStageRequest `json:"stage"`
	Artifacts       []WorkspaceFile       `json:"artifacts"`
}

// WorkspaceCleanupRequest removes one exact leased private snapshot.
type WorkspaceCleanupRequest struct {
	Version         int    `json:"version"`
	ContractVersion string `json:"contract_version"`
	Hash            string `json:"hash"`
	ManifestHash    string `json:"manifest_hash"`
	LeaseID         string `json:"lease_id"`
}

// NewWorkspacePublishRequest seals one remote publication request.
func NewWorkspacePublishRequest(stage WorkspaceStageRequest, artifacts []WorkspaceFile, leaseID string) (WorkspacePublishRequest, error) {
	request := WorkspacePublishRequest{
		Version: WorkspacePublishVersion, ContractVersion: WorkspacePublishContract,
		LeaseID: strings.TrimSpace(leaseID), Stage: stage, Artifacts: slices.Clone(artifacts),
	}
	hash, err := workspacePublishDigest(request)
	if err != nil {
		return WorkspacePublishRequest{}, err
	}
	request.Hash = hash
	if err := ValidateWorkspacePublishRequest(request); err != nil {
		return WorkspacePublishRequest{}, err
	}
	return request, nil
}

// ValidateWorkspacePublishRequest validates a credential-free remote snapshot request.
func ValidateWorkspacePublishRequest(request WorkspacePublishRequest) error {
	if request.Version != WorkspacePublishVersion || request.ContractVersion != WorkspacePublishContract || !workspaceLeaseIDPattern.MatchString(request.LeaseID) {
		return fmt.Errorf("workspace publish request identity is invalid")
	}
	if err := ValidateWorkspaceStageRequestIdentity(request.Stage); err != nil {
		return err
	}
	if request.Stage.InputMode != WorkspaceStageInputRemote {
		return fmt.Errorf("workspace publish request requires remote stage input")
	}
	artifactHash, artifactFiles, artifactBytes, err := WorkspaceArtifactIdentity(request.Artifacts)
	if err != nil || artifactHash != request.Stage.ArtifactManifestSHA256 || artifactFiles != request.Stage.ArtifactFiles || artifactBytes != request.Stage.ArtifactBytes {
		return fmt.Errorf("workspace publish artifact identity changed")
	}
	if !validSHA256(request.Hash) {
		return fmt.Errorf("workspace publish request hash is invalid")
	}
	digest, err := workspacePublishDigest(request)
	if err != nil || digest != request.Hash {
		return fmt.Errorf("workspace publish request hash changed")
	}
	data, err := json.Marshal(request)
	if err != nil || len(data) > WorkspacePublishRawMaxBytes {
		return fmt.Errorf("workspace publish request is oversized")
	}
	return nil
}

// NewWorkspaceCleanupRequest seals one exact leased cleanup request.
func NewWorkspaceCleanupRequest(manifestHash, leaseID string) (WorkspaceCleanupRequest, error) {
	request := WorkspaceCleanupRequest{
		Version: WorkspaceCleanupVersion, ContractVersion: WorkspaceCleanupContract,
		ManifestHash: strings.TrimSpace(manifestHash), LeaseID: strings.TrimSpace(leaseID),
	}
	hash, err := workspaceCleanupDigest(request)
	if err != nil {
		return WorkspaceCleanupRequest{}, err
	}
	request.Hash = hash
	if err := ValidateWorkspaceCleanupRequest(request); err != nil {
		return WorkspaceCleanupRequest{}, err
	}
	return request, nil
}

// ValidateWorkspaceCleanupRequest validates one content-addressed cleanup request.
func ValidateWorkspaceCleanupRequest(request WorkspaceCleanupRequest) error {
	if request.Version != WorkspaceCleanupVersion || request.ContractVersion != WorkspaceCleanupContract || !validSHA256(request.ManifestHash) || !workspaceLeaseIDPattern.MatchString(request.LeaseID) || !validSHA256(request.Hash) {
		return fmt.Errorf("workspace cleanup request identity is invalid")
	}
	digest, err := workspaceCleanupDigest(request)
	if err != nil || digest != request.Hash {
		return fmt.Errorf("workspace cleanup request hash changed")
	}
	return nil
}

func workspacePublishDigest(request WorkspacePublishRequest) (string, error) {
	request.Hash = ""
	data, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	return hashString(string(data)), nil
}

func workspaceCleanupDigest(request WorkspaceCleanupRequest) (string, error) {
	request.Hash = ""
	data, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	return hashString(string(data)), nil
}
