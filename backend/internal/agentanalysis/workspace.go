package agentanalysis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

const (
	WorkspaceManifestVersion = 1
	WorkspaceRequestVersion  = 1
	WorkspaceResultVersion   = 1
	WorkspaceStageVersion    = 1
	WorkspaceContractVersion = "agent-analysis-workspace-v2"
	WorkspaceStageContract   = "agent-analysis-stage-v1"
	WorkspacePromptVersion   = "agent-analysis-workspace-prompt-v2"

	WorkspaceSourceDir    = "source"
	WorkspaceArtifactsDir = "artifacts"
	WorkspaceResultDir    = "result"
	WorkspaceResultFile   = "analysis.json"

	maxWorkspaceFiles        = 512
	maxWorkspaceFileBytes    = int64(8 << 20)
	maxWorkspaceTotalBytes   = int64(32 << 20)
	maxWorkspacePromptBytes  = 32 << 10
	maxWorkspaceRequestBytes = 88 << 10
	maxWorkspaceStageBytes   = 95 << 10
)

// WorkspaceFile identifies one immutable artifact file exposed to OpenCode.
type WorkspaceFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// WorkspaceManifest seals one failure to a pinned source and artifact snapshot.
type WorkspaceManifest struct {
	Version         int                            `json:"version"`
	ContractVersion string                         `json:"contract_version"`
	Hash            string                         `json:"hash"`
	Request         ai.FailureAnalysisRequest      `json:"request"`
	Source          sourceinvestigation.Repository `json:"source"`
	ConsumerPrompt  string                         `json:"consumer_prompt"`
	Artifacts       []WorkspaceFile                `json:"artifacts"`
}

// WorkspaceStageRequest binds staging to the sealed source and artifact snapshot.
type WorkspaceStageRequest struct {
	Version         int                            `json:"version"`
	ContractVersion string                         `json:"contract_version"`
	Hash            string                         `json:"hash"`
	ManifestHash    string                         `json:"manifest_hash"`
	BuildPrefix     string                         `json:"build_prefix"`
	Source          sourceinvestigation.Repository `json:"source"`
	Artifacts       []WorkspaceFile                `json:"artifacts"`
}

// WorkspaceExecutionRequest is the non-secret request passed to one analyzer Sandbox.
type WorkspaceExecutionRequest struct {
	Version          int                              `json:"version"`
	ContractVersion  string                           `json:"contract_version"`
	Hash             string                           `json:"hash"`
	PromptVersion    string                           `json:"prompt_version"`
	PromptHash       string                           `json:"prompt_hash"`
	ResultSchemaHash string                           `json:"result_schema_hash"`
	Manifest         WorkspaceManifest                `json:"manifest"`
	ModelGateway     engineruntime.ModelGatewayConfig `json:"model_gateway"`
	TimeoutSeconds   int64                            `json:"timeout_seconds"`
	MaxSteps         int                              `json:"max_steps"`
	OutputLimitBytes int64                            `json:"output_limit_bytes"`
}

// NewWorkspaceManifest creates one deterministic file-backed analyzer input.
func NewWorkspaceManifest(request ai.FailureAnalysisRequest, source sourceinvestigation.Repository, consumerPrompt string, files []WorkspaceFile) (WorkspaceManifest, error) {
	source.Owner = strings.TrimSpace(source.Owner)
	source.Name = strings.TrimSpace(source.Name)
	source.Revision = strings.TrimSpace(source.Revision)
	manifest := WorkspaceManifest{
		Version: WorkspaceManifestVersion, ContractVersion: WorkspaceContractVersion,
		Request: canonicalRequest(request), Source: source,
		ConsumerPrompt: strings.TrimSpace(strings.ReplaceAll(consumerPrompt, "\r\n", "\n")),
		Artifacts:      slices.Clone(files),
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
	if err := sourceinvestigation.ValidateRepository(manifest.Source); err != nil || !immutableSourceRevision.MatchString(manifest.Source.Revision) {
		return fmt.Errorf("%w: source revision is invalid", ErrInvalidBundle)
	}
	if manifest.Request.JobID == "" || manifest.Request.BuildPrefix == "" || manifest.Request.Build.BuildID == "" || manifest.Request.TestCase.Name == "" || !requestStringsValid(manifest.Request) {
		return fmt.Errorf("%w: failure request is invalid", ErrInvalidBundle)
	}
	if canonical := canonicalRequest(manifest.Request); !requestsEqual(canonical, manifest.Request) {
		return fmt.Errorf("%w: failure request is not canonical", ErrInvalidBundle)
	}
	if manifest.Source.Owner != strings.TrimSpace(manifest.Source.Owner) || manifest.Source.Name != strings.TrimSpace(manifest.Source.Name) || manifest.Source.Revision != strings.TrimSpace(manifest.Source.Revision) {
		return fmt.Errorf("%w: source identity is not canonical", ErrInvalidBundle)
	}
	if manifest.ConsumerPrompt == "" || manifest.ConsumerPrompt != strings.TrimSpace(manifest.ConsumerPrompt) || !utf8StringWithin(manifest.ConsumerPrompt, maxWorkspacePromptBytes) {
		return fmt.Errorf("%w: consumer prompt is empty, invalid, or oversized", ErrInvalidBundle)
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

// NewWorkspaceStageRequest creates the exact stager input for one manifest.
func NewWorkspaceStageRequest(manifest WorkspaceManifest) (WorkspaceStageRequest, error) {
	if err := ValidateWorkspaceManifest(manifest); err != nil {
		return WorkspaceStageRequest{}, err
	}
	stage := WorkspaceStageRequest{
		Version: WorkspaceStageVersion, ContractVersion: WorkspaceStageContract,
		ManifestHash: manifest.Hash, BuildPrefix: manifest.Request.BuildPrefix,
		Source: manifest.Source, Artifacts: slices.Clone(manifest.Artifacts),
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
	if err := sourceinvestigation.ValidateRepository(stage.Source); err != nil || !immutableSourceRevision.MatchString(stage.Source.Revision) {
		return fmt.Errorf("workspace stage source identity is invalid")
	}
	if err := validateWorkspaceFiles(stage.Artifacts); err != nil {
		return err
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
	if stage.ManifestHash != manifest.Hash || stage.BuildPrefix != manifest.Request.BuildPrefix || stage.Source != manifest.Source || !slices.Equal(stage.Artifacts, manifest.Artifacts) {
		return fmt.Errorf("workspace stage request does not match the sealed manifest")
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

// VerifySourceWorkspace confirms the checkout exactly matches the pinned commit.
func VerifySourceWorkspace(ctx context.Context, root, revision string) error {
	if !immutableSourceRevision.MatchString(revision) {
		return fmt.Errorf("source revision is invalid")
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source workspace root is not a safe directory")
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
			return fmt.Errorf("source workspace uses unsupported index flags")
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
	for _, args := range [][]string{
		{"diff", "--cached", "--no-ext-diff", "--no-textconv", "--quiet", revision, "--"},
		{"diff", "--no-ext-diff", "--no-textconv", "--quiet", "--"},
	} {
		if _, err := gitWorkspaceOutput(ctx, root, args...); err != nil {
			return fmt.Errorf("source workspace tracked files changed")
		}
	}
	for _, args := range [][]string{{"ls-files", "--others", "--exclude-standard", "-z"}, {"ls-files", "--others", "--ignored", "--exclude-standard", "-z"}} {
		output, err := gitWorkspaceOutput(ctx, root, args...)
		if err != nil {
			return fmt.Errorf("inspect source workspace extras: %w", err)
		}
		if len(output) != 0 {
			return fmt.Errorf("source workspace contains untracked or ignored files")
		}
	}
	return nil
}

func gitWorkspaceOutput(ctx context.Context, root string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1")
	return command.CombinedOutput()
}

// NewWorkspaceExecutionRequest seals runtime bounds and gateway identity.
func NewWorkspaceExecutionRequest(manifest WorkspaceManifest, gateway engineruntime.ModelGatewayConfig, timeout time.Duration, maxSteps int, outputLimit int64) (WorkspaceExecutionRequest, error) {
	request := WorkspaceExecutionRequest{
		Version: WorkspaceRequestVersion, ContractVersion: WorkspaceContractVersion, PromptVersion: WorkspacePromptVersion,
		PromptHash: WorkspaceSkillHash(), ResultSchemaHash: WorkspaceResultSchemaHash(), Manifest: manifest, ModelGateway: gateway, TimeoutSeconds: int64(timeout.Round(time.Second) / time.Second),
		MaxSteps: maxSteps, OutputLimitBytes: outputLimit,
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

// ValidateWorkspaceExecutionRequest validates one credential-free analyzer request.
func ValidateWorkspaceExecutionRequest(request WorkspaceExecutionRequest) error {
	if request.Version != WorkspaceRequestVersion || request.ContractVersion != WorkspaceContractVersion || request.PromptVersion != WorkspacePromptVersion || request.PromptHash != WorkspaceSkillHash() || request.ResultSchemaHash != WorkspaceResultSchemaHash() {
		return fmt.Errorf("workspace analysis request version is invalid")
	}
	if err := ValidateWorkspaceManifest(request.Manifest); err != nil {
		return err
	}
	if strings.TrimSpace(request.ModelGateway.Model) == "" || request.ModelGateway.ProtocolVersion != "openai-chat-completions-v1" {
		return fmt.Errorf("workspace analysis model gateway is invalid")
	}
	if err := engineruntime.ValidateModelGatewayTrust(request.ModelGateway.Endpoint, false); err != nil {
		return fmt.Errorf("workspace analysis model gateway: %w", err)
	}
	if request.TimeoutSeconds < 1 || request.TimeoutSeconds > int64((30*time.Minute)/time.Second) {
		return fmt.Errorf("workspace analysis timeout must be between 1 second and 30 minutes")
	}
	if request.MaxSteps < 1 || request.MaxSteps > 100 {
		return fmt.Errorf("workspace analysis max steps must be between 1 and 100")
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
