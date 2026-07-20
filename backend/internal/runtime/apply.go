package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxRemoteDiffBytes         = 4 << 20
	maxRemoteFileContentBytes  = 2 << 20
	maxRemoteTotalContentBytes = 8 << 20
)

// ApplyDiff applies a remote agent diff to the pinned repository snapshot and
// reconstructs the changed-file map used by the normal fix-PR guardrails.
func ApplyDiff(ctx context.Context, repo RepoRef, diff string) (map[string]string, string, error) {
	if strings.TrimSpace(diff) == "" {
		return map[string]string{}, "", nil
	}
	if len(diff) > maxRemoteDiffBytes {
		return nil, "", fmt.Errorf("runtime: remote diff is %d bytes, exceeds %d", len(diff), maxRemoteDiffBytes)
	}
	work, err := os.MkdirTemp("", "pad-apply-*")
	if err != nil {
		return nil, "", fmt.Errorf("runtime: temp dir: %w", err)
	}
	defer os.RemoveAll(work)

	if err := materialize(ctx, work, repo); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, "", fmt.Errorf("%w: clone timed out", ErrUnavailable)
		}
		return nil, "", err
	}

	patch, err := os.CreateTemp("", "pad-fix-*.patch")
	if err != nil {
		return nil, "", fmt.Errorf("runtime: temp patch: %w", err)
	}
	defer os.Remove(patch.Name())
	if _, err := patch.WriteString(diff); err != nil {
		_ = patch.Close()
		return nil, "", fmt.Errorf("runtime: write patch: %w", err)
	}
	if err := patch.Close(); err != nil {
		return nil, "", fmt.Errorf("runtime: write patch: %w", err)
	}
	if err := gitRun(ctx, work, "apply", "--check", patch.Name()); err != nil {
		return nil, "", fmt.Errorf("runtime: check remote diff against pinned base: %w", err)
	}
	if err := gitRun(ctx, work, "apply", "--whitespace=nowarn", patch.Name()); err != nil {
		return nil, "", fmt.Errorf("runtime: apply remote diff: %w", err)
	}
	if err := gitRun(ctx, work, "add", "-A"); err != nil {
		return nil, "", err
	}
	if err := validateRemoteChange(ctx, work); err != nil {
		return nil, "", err
	}
	if err := validateRemoteContentSizes(ctx, work); err != nil {
		return nil, "", err
	}
	return gitChanges(ctx, work, repo.Token)
}

func validateRemoteContentSizes(ctx context.Context, work string) error {
	names, err := gitOut(ctx, work, "diff", "--cached", "--name-only", "--diff-filter=AM")
	if err != nil {
		return err
	}
	var total int64
	for _, file := range strings.Split(strings.TrimSpace(names), "\n") {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		info, err := os.Stat(filepath.Join(work, file))
		if err != nil {
			return fmt.Errorf("runtime: stat changed %s: %w", file, err)
		}
		if info.Size() > maxRemoteFileContentBytes {
			return fmt.Errorf("runtime: changed file %s is %d bytes, exceeds %d", file, info.Size(), maxRemoteFileContentBytes)
		}
		total += info.Size()
		if total > maxRemoteTotalContentBytes {
			return fmt.Errorf("runtime: changed file content is %d bytes, exceeds %d", total, maxRemoteTotalContentBytes)
		}
	}
	return nil
}

func validateRemoteChange(ctx context.Context, work string) error {
	status, err := gitOut(ctx, work, "diff", "--cached", "--name-status")
	if err != nil {
		return err
	}
	var changed []string
	for _, line := range strings.Split(strings.TrimSpace(status), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) < 2 || parts[0] == "" {
			continue
		}
		kind := parts[0][0]
		if strings.ContainsRune("DRTCU", rune(kind)) {
			return fmt.Errorf("runtime: remote diff contains unsupported %c change for %s", kind, parts[1])
		}
		file := parts[len(parts)-1]
		if file == ".gitmodules" {
			return fmt.Errorf("runtime: remote diff may not modify .gitmodules")
		}
		changed = append(changed, file)
	}
	numstat, err := gitOut(ctx, work, "diff", "--cached", "--numstat")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(numstat), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && (fields[0] == "-" || fields[1] == "-") {
			return fmt.Errorf("runtime: remote diff contains a binary file")
		}
	}
	for _, file := range changed {
		entry, err := gitOut(ctx, work, "ls-files", "-s", "--", file)
		if err != nil {
			return err
		}
		fields := strings.Fields(entry)
		if len(fields) > 0 && (fields[0] == "120000" || fields[0] == "160000") {
			return fmt.Errorf("runtime: remote diff contains an unsupported symlink or submodule at %s", file)
		}
	}
	summary, err := gitOut(ctx, work, "diff", "--cached", "--summary")
	if err != nil {
		return err
	}
	if strings.Contains(summary, "mode change") {
		return fmt.Errorf("runtime: remote diff contains an unsupported mode change")
	}
	return nil
}
