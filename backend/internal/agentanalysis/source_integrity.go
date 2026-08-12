package agentanalysis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	SourceStagedContentChanged   = "source_staged_content_changed"
	SourceWorktreeContentChanged = "source_worktree_content_changed"
	SourceWorktreeModeChanged    = "source_worktree_mode_changed"
	SourceIndexFlagsChanged      = "source_index_flags_changed"
	SourceIndexModeChanged       = "source_index_mode_changed"
	SourceModePolicyChanged      = "source_mode_policy_changed"
	SourceUntrackedFiles         = "source_untracked_files"
	SourceGitDiffError           = "source_git_diff_error"
)

type sourceIntegrityError struct {
	category string
	cause    error
}

func (e *sourceIntegrityError) Error() string { return e.category }
func (e *sourceIntegrityError) Unwrap() error { return e.cause }

func sourceIntegrityFailure(category string, cause error) error {
	return &sourceIntegrityError{category: category, cause: cause}
}

// SourceIntegrityCategory returns the privacy-safe source verification category.
func SourceIntegrityCategory(err error) string {
	var integrity *sourceIntegrityError
	if errors.As(err, &integrity) {
		return integrity.category
	}
	return ""
}

// SourceIntegrityGitExitCodes records bounded Git comparison outcomes.
type SourceIntegrityGitExitCodes struct {
	Staged          int `json:"staged"`
	WorktreeContent int `json:"worktree_content"`
	WorktreeAll     int `json:"worktree_all"`
}

// SourceIntegritySnapshot records content-free source workspace facts.
type SourceIntegritySnapshot struct {
	Head                   string                      `json:"head"`
	Tree                   string                      `json:"tree"`
	ModePolicy             WorkspaceSourceModePolicy   `json:"mode_policy"`
	CoreFileMode           string                      `json:"core_filemode"`
	IndexModeRegular       int                         `json:"index_mode_100644"`
	IndexModeExecutable    int                         `json:"index_mode_100755"`
	IndexModeSymlink       int                         `json:"index_mode_120000"`
	IndexModeOther         int                         `json:"index_mode_other"`
	IndexFlagsNormal       int                         `json:"index_flags_normal"`
	IndexFlagsChanged      int                         `json:"index_flags_changed"`
	PorcelainV2Entries     int                         `json:"porcelain_v2_entries"`
	PorcelainV2SHA256      string                      `json:"porcelain_v2_sha256"`
	StagedContentChanges   int                         `json:"staged_content_changes"`
	StagedModeChanges      int                         `json:"staged_mode_changes"`
	WorktreeContentChanges int                         `json:"worktree_content_changes"`
	WorktreeModeChanges    int                         `json:"worktree_mode_changes"`
	UntrackedFiles         int                         `json:"untracked_files"`
	IgnoredFiles           int                         `json:"ignored_files"`
	GitExitCodes           SourceIntegrityGitExitCodes `json:"git_exit_codes"`
}

// InspectPreparedSourceIntegrity returns content-free source workspace facts.
func InspectPreparedSourceIntegrity(ctx context.Context, root, revision string, modePolicy WorkspaceSourceModePolicy) (SourceIntegritySnapshot, error) {
	var snapshot SourceIntegritySnapshot
	root = filepath.Clean(root)
	snapshot.ModePolicy = modePolicy

	configured, err := gitWorkspaceOutput(ctx, root, "config", "--local", "--bool", "--get", "core.filemode")
	if err != nil {
		return snapshot, fmt.Errorf("inspect source mode policy: %w", err)
	}
	snapshot.CoreFileMode = strings.TrimSpace(string(configured))
	if output, err := gitWorkspaceOutput(ctx, root, "rev-parse", "HEAD"); err != nil {
		return snapshot, fmt.Errorf("inspect source HEAD: %w", err)
	} else {
		snapshot.Head = strings.TrimSpace(string(output))
	}
	if output, err := gitWorkspaceOutput(ctx, root, "rev-parse", "HEAD^{tree}"); err != nil {
		return snapshot, fmt.Errorf("inspect source tree: %w", err)
	} else {
		snapshot.Tree = strings.TrimSpace(string(output))
	}

	flags, err := gitWorkspaceOutput(ctx, root, "ls-files", "-v", "-z")
	if err != nil {
		return snapshot, fmt.Errorf("inspect source index flags: %w", err)
	}
	for _, record := range bytes.Split(flags, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		if len(record) >= 2 && record[0] == 'H' && record[1] == ' ' {
			snapshot.IndexFlagsNormal++
		} else {
			snapshot.IndexFlagsChanged++
		}
	}
	staged, err := gitWorkspaceOutput(ctx, root, "ls-files", "--stage", "-z")
	if err != nil {
		return snapshot, fmt.Errorf("inspect source index modes: %w", err)
	}
	for _, record := range bytes.Split(staged, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		mode, _, parseErr := sourceIndexEntry(record)
		if parseErr != nil {
			return snapshot, parseErr
		}
		switch mode {
		case "100644":
			snapshot.IndexModeRegular++
		case "100755":
			snapshot.IndexModeExecutable++
		case "120000":
			snapshot.IndexModeSymlink++
		default:
			snapshot.IndexModeOther++
		}
	}

	status, err := gitWorkspaceOutput(ctx, root, "status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return snapshot, fmt.Errorf("inspect source porcelain status: %w", err)
	}
	redacted := make([]string, 0)
	for _, record := range bytes.Split(status, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		snapshot.PorcelainV2Entries++
		fields := strings.Fields(string(record))
		kind := string(record[0])
		if kind == "?" {
			snapshot.UntrackedFiles++
		} else if kind == "!" {
			snapshot.IgnoredFiles++
		}
		if len(fields) >= 3 && (kind == "1" || kind == "2" || kind == "u") {
			redacted = append(redacted, strings.Join(fields[:3], " "))
		} else {
			redacted = append(redacted, kind)
		}
	}
	sort.Strings(redacted)
	digest := sha256.Sum256([]byte(strings.Join(redacted, "\n")))
	snapshot.PorcelainV2SHA256 = hex.EncodeToString(digest[:])

	snapshot.GitExitCodes.Staged, err = sourceDiffExitCode(ctx, root, "diff", "--cached", "--no-ext-diff", "--no-textconv", "--quiet", revision, "--")
	if err != nil {
		return snapshot, err
	}
	snapshot.GitExitCodes.WorktreeContent, err = sourceDiffExitCode(ctx, root, "-c", "core.filemode=false", "diff", "--no-ext-diff", "--no-textconv", "--quiet", "--")
	if err != nil {
		return snapshot, err
	}
	snapshot.GitExitCodes.WorktreeAll, err = sourceDiffExitCode(ctx, root, "diff", "--no-ext-diff", "--no-textconv", "--quiet", "--")
	if err != nil {
		return snapshot, err
	}

	snapshot.StagedContentChanges, snapshot.StagedModeChanges, err = stagedDiffCounts(ctx, root, revision)
	if err != nil {
		return snapshot, err
	}
	if output, commandErr := gitWorkspaceOutput(ctx, root, "-c", "core.filemode=false", "diff", "--name-only", "-z", "--"); commandErr == nil {
		snapshot.WorktreeContentChanges = countNULTerminated(output)
	} else {
		return snapshot, sourceIntegrityFailure(SourceGitDiffError, commandErr)
	}
	if output, commandErr := gitWorkspaceOutput(ctx, root, "diff", "--summary", "--"); commandErr == nil {
		for _, line := range strings.Split(string(output), "\n") {
			if strings.Contains(line, "mode change") {
				snapshot.WorktreeModeChanges++
			}
		}
	} else {
		return snapshot, sourceIntegrityFailure(SourceGitDiffError, commandErr)
	}
	return snapshot, nil
}

func stagedDiffCounts(ctx context.Context, root, revision string) (int, int, error) {
	output, err := gitWorkspaceOutput(ctx, root, "diff", "--cached", "--raw", "--no-renames", "-z", revision, "--")
	if err != nil {
		return 0, 0, sourceIntegrityFailure(SourceGitDiffError, err)
	}
	records := bytes.Split(output, []byte{0})
	contentChanges, modeChanges := 0, 0
	for index := 0; index < len(records); {
		metadata := records[index]
		index++
		if len(metadata) == 0 {
			break
		}
		if metadata[0] != ':' {
			return 0, 0, sourceIntegrityFailure(SourceGitDiffError, fmt.Errorf("staged diff metadata is malformed"))
		}
		fields := bytes.Fields(metadata[1:])
		if len(fields) != 5 || index >= len(records) || len(records[index]) == 0 {
			return 0, 0, sourceIntegrityFailure(SourceGitDiffError, fmt.Errorf("staged diff record is malformed"))
		}
		index++ // Consume the path without retaining it.
		oldMode, newMode := string(fields[0]), string(fields[1])
		oldObject, newObject := string(fields[2]), string(fields[3])
		status := string(fields[4])
		if oldMode != newMode {
			modeChanges++
		}
		if oldObject != newObject || status == "" || status[0] != 'M' {
			contentChanges++
		}
	}
	return contentChanges, modeChanges, nil
}

func countNULTerminated(output []byte) int {
	count := 0
	for _, record := range bytes.Split(output, []byte{0}) {
		if len(record) > 0 {
			count++
		}
	}
	return count
}

func sourceDiffExitCode(ctx context.Context, root string, args ...string) (int, error) {
	_, exitCode, err := gitWorkspaceOutputWithExitCode(ctx, root, args...)
	if err != nil || exitCode < 0 || exitCode > 1 {
		if err == nil {
			err = fmt.Errorf("git diff exited %s", strconv.Itoa(exitCode))
		}
		return exitCode, sourceIntegrityFailure(SourceGitDiffError, err)
	}
	return exitCode, nil
}

func gitWorkspaceOutputWithExitCode(ctx context.Context, root string, args ...string) ([]byte, int, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	command.Env = append(workspaceGitEnvironment(), "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1")
	output, err := command.CombinedOutput()
	if err == nil {
		return output, 0, nil
	}
	if ctx.Err() != nil {
		return output, -1, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return output, exitErr.ExitCode(), nil
	}
	return output, -1, err
}
