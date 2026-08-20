// Package analysisstager materializes one sealed analyzer workspace.
package analysisstager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/willie-yao/aster/backend/internal/agentanalysis"
)

const (
	defaultInputRoot     = "/input"
	defaultWorkspaceRoot = "/workspace"
	defaultRequestRoot   = agentanalysis.WorkspaceExecutionRequestRoot
)

// Options configure one staging process.
type Options struct {
	InputRoot     string
	WorkspaceRoot string
	RequestRoot   string
}

// Execute verifies one manifest-addressed snapshot and copies it into the workspace.
func Execute(ctx context.Context, request agentanalysis.WorkspaceStageRequest, execution agentanalysis.WorkspaceExecutionRequest, opts Options) error {
	if err := agentanalysis.ValidateWorkspaceExecutionRequest(execution); err != nil {
		return err
	}
	if err := agentanalysis.ValidateWorkspaceStageRequest(request, execution.Manifest); err != nil {
		return err
	}
	if execution.InputMode != agentanalysis.WorkspaceInputStaged || !slices.Equal(execution.SourceModePolicies, request.OutputSourceModePolicies) {
		return fmt.Errorf("workspace execution and stage requests are inconsistent")
	}
	if request.InputMode != agentanalysis.WorkspaceStageInputPVC {
		return fmt.Errorf("workspace staging requires PVC input")
	}
	inputRoot := strings.TrimSpace(opts.InputRoot)
	if inputRoot == "" {
		inputRoot = defaultInputRoot
	}
	workspaceRoot := strings.TrimSpace(opts.WorkspaceRoot)
	if workspaceRoot == "" {
		workspaceRoot = defaultWorkspaceRoot
	}
	requestRoot := strings.TrimSpace(opts.RequestRoot)
	if requestRoot == "" {
		requestRoot = defaultRequestRoot
	}
	inputRoot = filepath.Clean(inputRoot)
	workspaceRoot = filepath.Clean(workspaceRoot)
	requestRoot = filepath.Clean(requestRoot)
	if err := requireEmptyDirectory(workspaceRoot); err != nil {
		return err
	}
	lock, err := lockSnapshotReadOnly(inputRoot, request.ManifestHash)
	if err != nil {
		return fmt.Errorf("lock staged snapshot: %w", err)
	}
	defer unlockSnapshot(lock)
	snapshotRoot := filepath.Join(inputRoot, request.ManifestHash)
	sourcesInput := filepath.Join(snapshotRoot, agentanalysis.WorkspaceSourcesDir)
	artifactInput := filepath.Join(snapshotRoot, agentanalysis.WorkspaceArtifactsDir)
	artifacts, err := agentanalysis.ReadWorkspaceArtifactManifest(snapshotRoot, request)
	if err != nil {
		return err
	}
	for _, source := range request.Sources {
		sourceInput := filepath.Join(sourcesInput, source.ID)
		gitDir, err := os.Lstat(filepath.Join(sourceInput, ".git"))
		if err != nil || !gitDir.IsDir() || gitDir.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("staged source %s must contain a standalone Git directory", source.ID)
		}
		inputPolicy, _ := agentanalysis.WorkspaceSourceModeFor(request.InputSourceModePolicies, source.ID)
		if err := agentanalysis.VerifyPreparedSourceWorkspace(ctx, sourceInput, source.Repository.Revision, inputPolicy); err != nil {
			return fmt.Errorf("verify staged source %s: %w", source.ID, err)
		}
	}
	if err := agentanalysis.ValidateAggregateWorkspaceSources(ctx, sourcesInput); err != nil {
		return fmt.Errorf("inspect staged sources: %w", err)
	}
	sourcesOutput := filepath.Join(workspaceRoot, agentanalysis.WorkspaceSourcesDir)
	artifactOutput := filepath.Join(workspaceRoot, agentanalysis.WorkspaceArtifactsDir)
	resultOutput := filepath.Join(workspaceRoot, agentanalysis.WorkspaceResultDir)
	if err := os.Mkdir(sourcesOutput, 0o700); err != nil {
		return fmt.Errorf("create copied sources root: %w", err)
	}
	for _, source := range request.Sources {
		sourceInput := filepath.Join(sourcesInput, source.ID)
		sourceOutput := filepath.Join(sourcesOutput, source.ID)
		if err := cloneSource(ctx, sourceInput, sourceOutput, source.Repository.Revision); err != nil {
			return fmt.Errorf("clone staged source %s: %w", source.ID, err)
		}
		modePolicy, err := agentanalysis.ConfigurePreparedSourceModePolicy(ctx, sourceOutput, source.Repository.Revision)
		if err != nil {
			return fmt.Errorf("configure copied source %s mode policy: %w", source.ID, err)
		}
		expectedPolicy, _ := agentanalysis.WorkspaceSourceModeFor(request.OutputSourceModePolicies, source.ID)
		if modePolicy != expectedPolicy {
			return fmt.Errorf("copied source %s mode policy does not match the sealed request", source.ID)
		}
	}
	if err := agentanalysis.ValidateAggregateWorkspaceSources(ctx, sourcesOutput); err != nil {
		return fmt.Errorf("inspect copied sources: %w", err)
	}
	if err := copyArtifactTree(ctx, artifactInput, artifactOutput, artifacts); err != nil {
		return fmt.Errorf("copy staged artifacts: %w", err)
	}
	if err := os.Mkdir(resultOutput, 0o700); err != nil {
		return fmt.Errorf("create analysis result directory: %w", err)
	}
	if err := agentanalysis.WriteWorkspaceExecutionRequestFile(requestRoot, execution); err != nil {
		return fmt.Errorf("write workspace execution request: %w", err)
	}
	return nil
}

// WriteExecutionRequest writes one validated request into the fixed request volume.
func WriteExecutionRequest(request agentanalysis.WorkspaceExecutionRequest, opts Options) error {
	requestRoot := strings.TrimSpace(opts.RequestRoot)
	if requestRoot == "" {
		requestRoot = defaultRequestRoot
	}
	return agentanalysis.WriteWorkspaceExecutionRequestFile(filepath.Clean(requestRoot), request)
}

func requireEmptyDirectory(root string) error {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("workspace root is not a safe directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("workspace root must be empty")
	}
	return nil
}

func cloneSource(ctx context.Context, source, destination, revision string) error {
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	if output, err := runGit(ctx, "-C", destination, "init", "--quiet"); err != nil {
		return fmt.Errorf("initialize source: %v: %s", err, boundedOutput(output))
	}
	if output, err := runGit(ctx, "-C", destination, "fetch", "--depth=1", "--no-tags", source, revision); err != nil {
		return fmt.Errorf("fetch pinned source: %v: %s", err, boundedOutput(output))
	}
	if output, err := runGit(ctx, "-C", destination, "checkout", "--detach", "--quiet", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("checkout source: %v: %s", err, boundedOutput(output))
	}
	if err := os.Remove(filepath.Join(destination, ".git", "FETCH_HEAD")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove fetch metadata: %w", err)
	}
	return nil
}

func runGit(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	return command.CombinedOutput()
}

func boundedOutput(value []byte) string {
	text := strings.Join(strings.Fields(string(value)), " ")
	if len(text) > 1024 {
		text = text[:1024]
	}
	return text
}

func copyArtifactTree(ctx context.Context, source, destination string, expected []agentanalysis.WorkspaceFile) error {
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source root is not a safe directory")
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	files := make(map[string]agentanalysis.WorkspaceFile, len(expected))
	for _, file := range expected {
		files[file.Path] = file
	}
	copied := 0
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == source {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("input tree contains symlink %s", path)
		}
		relative, err := filepath.Rel(source, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("input tree path escapes the root")
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.Mkdir(target, 0o700)
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		sealed, ok := files[relative]
		if !ok {
			return fmt.Errorf("input tree contains unexpected file %s", relative)
		}
		if !fileInfo.Mode().IsRegular() || fileInfo.Size() != sealed.Size {
			return fmt.Errorf("input tree file %s does not match the sealed manifest", relative)
		}
		if err := copyVerifiedArtifact(path, target, fileInfo.Mode(), sealed); err != nil {
			return fmt.Errorf("copy %s: %w", relative, err)
		}
		delete(files, relative)
		copied++
		return nil
	})
	if err != nil {
		return err
	}
	if copied != len(expected) || len(files) != 0 {
		return fmt.Errorf("input tree is missing sealed artifact files")
	}
	return nil
}

func copyVerifiedArtifact(source, destination string, mode os.FileMode, expected agentanalysis.WorkspaceFile) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	outputMode := os.FileMode(0o600)
	if mode&0o111 != 0 {
		outputMode = 0o700
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, outputMode)
	if err != nil {
		return err
	}
	hash := sha256.New()
	copied, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(input, expected.Size+1))
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if copied != expected.Size || hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		return fmt.Errorf("artifact bytes do not match the sealed manifest")
	}
	return nil
}
