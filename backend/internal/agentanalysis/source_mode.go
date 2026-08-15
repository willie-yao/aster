package agentanalysis

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// WorkspaceSourceModePolicy defines how the prepared filesystem represents Git executable bits.
type WorkspaceSourceModePolicy string

const (
	// WorkspaceSourceModePreserve requires worktree executable bits to match the Git index.
	WorkspaceSourceModePreserve WorkspaceSourceModePolicy = "preserve"
	// WorkspaceSourceModeIgnoreExecutable requires core.filemode=false and validates content independently of worktree mode bits.
	WorkspaceSourceModeIgnoreExecutable WorkspaceSourceModePolicy = "ignore_executable_bit"
)

func validWorkspaceSourceModePolicy(policy WorkspaceSourceModePolicy) bool {
	return policy == WorkspaceSourceModePreserve || policy == WorkspaceSourceModeIgnoreExecutable
}

// ConfigurePreparedSourceModePolicy probes the populated filesystem and seals the matching repository-local Git policy.
func ConfigurePreparedSourceModePolicy(ctx context.Context, root, revision string) (WorkspaceSourceModePolicy, error) {
	return configurePreparedSourceModePolicy(ctx, root, revision, preparedFilesystemPreservesExecutableMode)
}

func configurePreparedSourceModePolicy(ctx context.Context, root, revision string, preservesExecutableMode func(string) (bool, error)) (WorkspaceSourceModePolicy, error) {
	root = filepath.Clean(root)
	gitDir, err := os.Lstat(filepath.Join(root, ".git"))
	if err != nil || !gitDir.IsDir() || gitDir.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("prepared source must contain a standalone Git directory")
	}
	previous, err := preparedSourceFileMode(ctx, root)
	if err != nil {
		return "", err
	}
	restore := func(cause error) error {
		if err := setPreparedSourceFileMode(ctx, root, previous); err != nil {
			return errors.Join(cause, fmt.Errorf("restore prepared source mode policy: %w", err))
		}
		return cause
	}
	if err := SetPreparedSourceModePolicy(ctx, root, WorkspaceSourceModeIgnoreExecutable); err != nil {
		return "", err
	}
	if err := VerifyPreparedSourceWorkspace(ctx, root, revision, WorkspaceSourceModeIgnoreExecutable); err != nil {
		return "", restore(fmt.Errorf("verify prepared source content before mode sealing: %w", err))
	}
	policy, err := detectPreparedSourceModePolicy(ctx, root, preservesExecutableMode)
	if err != nil {
		return "", restore(err)
	}
	if err := SetPreparedSourceModePolicy(ctx, root, policy); err != nil {
		return "", restore(err)
	}
	if err := VerifyPreparedSourceWorkspace(ctx, root, revision, policy); err != nil {
		return "", restore(fmt.Errorf("verify prepared source after mode sealing: %w", err))
	}
	return policy, nil
}

// SetPreparedSourceModePolicy writes only repository-local core.filemode.
func SetPreparedSourceModePolicy(ctx context.Context, root string, policy WorkspaceSourceModePolicy) error {
	if !validWorkspaceSourceModePolicy(policy) {
		return fmt.Errorf("workspace source mode policy is invalid")
	}
	value := true
	if policy == WorkspaceSourceModeIgnoreExecutable {
		value = false
	}
	return setPreparedSourceFileMode(ctx, root, value)
}

func preparedSourceFileMode(ctx context.Context, root string) (bool, error) {
	output, err := gitWorkspaceOutput(ctx, root, "config", "--local", "--bool", "--get", "core.filemode")
	if err != nil {
		return false, fmt.Errorf("inspect prepared source mode policy: %w", err)
	}
	switch strings.TrimSpace(string(output)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("prepared source mode policy is invalid")
	}
}

func setPreparedSourceFileMode(ctx context.Context, root string, value bool) error {
	text := "false"
	if value {
		text = "true"
	}
	if output, err := gitWorkspaceOutput(ctx, root, "config", "--local", "core.filemode", text); err != nil {
		return fmt.Errorf("configure prepared source mode policy: %w: %s", err, boundedGitOutput(output))
	}
	return nil
}

func detectPreparedSourceModePolicy(ctx context.Context, root string, preservesExecutableMode func(string) (bool, error)) (WorkspaceSourceModePolicy, error) {
	preserves, err := preservesExecutableMode(root)
	if err != nil {
		return "", err
	}
	mismatch, err := preparedSourceHasExecutableModeMismatch(ctx, root)
	if err != nil {
		return "", err
	}
	if !preserves {
		return WorkspaceSourceModeIgnoreExecutable, nil
	}
	if mismatch {
		return "", fmt.Errorf("prepared source executable modes differ on a mode-preserving filesystem")
	}
	return WorkspaceSourceModePreserve, nil
}

func preparedSourceHasExecutableModeMismatch(ctx context.Context, root string) (bool, error) {
	staged, err := gitWorkspaceOutput(ctx, root, "ls-files", "--stage", "-z")
	if err != nil {
		return false, fmt.Errorf("inspect prepared source index modes: %w", err)
	}
	for _, record := range bytes.Split(staged, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		mode, path, err := sourceIndexEntry(record)
		if err != nil {
			return false, err
		}
		if mode != "100644" && mode != "100755" {
			continue
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil || !info.Mode().IsRegular() {
			return false, fmt.Errorf("prepared source index path is unavailable")
		}
		if (mode == "100755") != (info.Mode().Perm()&0o111 != 0) {
			return true, nil
		}
	}
	return false, nil
}

func preparedFilesystemPreservesExecutableMode(root string) (bool, error) {
	probe, err := os.CreateTemp(filepath.Join(root, ".git"), "prow-filemode-probe-")
	if err != nil {
		return false, fmt.Errorf("create prepared source mode probe: %w", err)
	}
	probePath := probe.Name()
	if closeErr := probe.Close(); closeErr != nil {
		_ = os.Remove(probePath)
		return false, fmt.Errorf("close prepared source mode probe: %w", closeErr)
	}
	defer os.Remove(probePath)
	for _, check := range []struct {
		mode       fs.FileMode
		executable bool
	}{{mode: 0o600, executable: false}, {mode: 0o700, executable: true}} {
		if err := os.Chmod(probePath, check.mode); err != nil {
			if unsupportedModeChange(err) {
				return false, nil
			}
			return false, fmt.Errorf("change prepared source mode probe: %w", err)
		}
		info, err := os.Stat(probePath)
		if err != nil {
			return false, fmt.Errorf("inspect prepared source mode probe: %w", err)
		}
		if (info.Mode().Perm()&0o111 != 0) != check.executable {
			return false, nil
		}
	}
	return true, nil
}

func unsupportedModeChange(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}

func boundedGitOutput(value []byte) string {
	text := strings.Join(strings.Fields(string(value)), " ")
	if len(text) > 512 {
		text = text[:512]
	}
	return text
}
