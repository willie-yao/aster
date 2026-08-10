// Package analysisstager materializes one sealed analyzer workspace.
package analysisstager

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
)

const (
	defaultInputRoot     = "/input"
	defaultWorkspaceRoot = "/workspace"
	maxSourceFiles       = 100_000
	maxSourceFileBytes   = int64(512 << 20)
	maxSourceTotalBytes  = int64(2 << 30)
)

// Options configure one staging process.
type Options struct {
	InputRoot     string
	WorkspaceRoot string
}

// Execute verifies one manifest-addressed snapshot and copies it into the workspace.
func Execute(ctx context.Context, request agentanalysis.WorkspaceStageRequest, opts Options) error {
	if err := agentanalysis.ValidateWorkspaceStageRequestIdentity(request); err != nil {
		return err
	}
	inputRoot := strings.TrimSpace(opts.InputRoot)
	if inputRoot == "" {
		inputRoot = defaultInputRoot
	}
	workspaceRoot := strings.TrimSpace(opts.WorkspaceRoot)
	if workspaceRoot == "" {
		workspaceRoot = defaultWorkspaceRoot
	}
	inputRoot = filepath.Clean(inputRoot)
	workspaceRoot = filepath.Clean(workspaceRoot)
	if err := requireEmptyDirectory(workspaceRoot); err != nil {
		return err
	}
	snapshotRoot := filepath.Join(inputRoot, request.ManifestHash)
	sourceInput := filepath.Join(snapshotRoot, agentanalysis.WorkspaceSourceDir)
	artifactInput := filepath.Join(snapshotRoot, agentanalysis.WorkspaceArtifactsDir)
	gitDir, err := os.Lstat(filepath.Join(sourceInput, ".git"))
	if err != nil || !gitDir.IsDir() || gitDir.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("staged source must contain a standalone Git directory")
	}
	if err := agentanalysis.VerifySourceWorkspace(ctx, sourceInput, request.Source.Revision); err != nil {
		return fmt.Errorf("verify staged source: %w", err)
	}
	if err := agentanalysis.VerifyArtifactFiles(artifactInput, request.Artifacts); err != nil {
		return fmt.Errorf("verify staged artifacts: %w", err)
	}
	sourceOutput := filepath.Join(workspaceRoot, agentanalysis.WorkspaceSourceDir)
	artifactOutput := filepath.Join(workspaceRoot, agentanalysis.WorkspaceArtifactsDir)
	resultOutput := filepath.Join(workspaceRoot, agentanalysis.WorkspaceResultDir)
	if _, _, err := inspectTree(ctx, sourceInput, maxSourceFiles, maxSourceFileBytes, maxSourceTotalBytes); err != nil {
		return fmt.Errorf("inspect staged source: %w", err)
	}
	if err := cloneSource(ctx, sourceInput, sourceOutput, request.Source.Revision); err != nil {
		return fmt.Errorf("clone staged source: %w", err)
	}
	if _, _, err := copyTree(ctx, artifactInput, artifactOutput, len(request.Artifacts), 8<<20, 32<<20); err != nil {
		return fmt.Errorf("copy staged artifacts: %w", err)
	}
	if err := os.Mkdir(resultOutput, 0o700); err != nil {
		return fmt.Errorf("create analysis result directory: %w", err)
	}
	if err := agentanalysis.VerifySourceWorkspace(ctx, sourceOutput, request.Source.Revision); err != nil {
		return fmt.Errorf("verify copied source: %w", err)
	}
	if err := agentanalysis.VerifyArtifactFiles(artifactOutput, request.Artifacts); err != nil {
		return fmt.Errorf("verify copied artifacts: %w", err)
	}
	return nil
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

func inspectTree(ctx context.Context, source string, maxFiles int, maxFileBytes, maxTotalBytes int64) (int, int64, error) {
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, 0, fmt.Errorf("source root is not a safe directory")
	}
	files := 0
	var total int64
	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == source || entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("input tree contains symlink %s", path)
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return err
		}
		if !fileInfo.Mode().IsRegular() || fileInfo.Size() > maxFileBytes {
			return fmt.Errorf("input tree contains an unsupported or oversized file")
		}
		files++
		total += fileInfo.Size()
		if files > maxFiles || total > maxTotalBytes {
			return fmt.Errorf("input tree exceeds file or byte bounds")
		}
		return nil
	})
	return files, total, err
}

func copyTree(ctx context.Context, source, destination string, maxFiles int, maxFileBytes, maxTotalBytes int64) (int, int64, error) {
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, 0, fmt.Errorf("source root is not a safe directory")
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return 0, 0, err
	}
	files := 0
	var total int64
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
		if !fileInfo.Mode().IsRegular() || fileInfo.Size() > maxFileBytes {
			return fmt.Errorf("input tree contains unsupported or oversized file %s", relative)
		}
		files++
		total += fileInfo.Size()
		if files > maxFiles || total > maxTotalBytes {
			return fmt.Errorf("input tree exceeds file or byte bounds")
		}
		return copyFile(path, target, fileInfo.Mode(), maxFileBytes)
	})
	return files, total, err
}

func copyFile(source, destination string, mode os.FileMode, limit int64) error {
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
	copied, copyErr := io.Copy(output, io.LimitReader(input, limit+1))
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if copied > limit {
		return fmt.Errorf("input file exceeds %d bytes", limit)
	}
	return closeErr
}
