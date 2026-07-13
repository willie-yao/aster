package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	defaultTimeout = 10 * time.Minute
	// maxOutputBytes bounds captured command output so a chatty build cannot
	// blow up the PR body or preview. The tail is kept, where errors surface.
	maxOutputBytes = 32 * 1024
	// waitDelay bounds how long Wait blocks for output pipes after the command
	// is killed on timeout, so a descendant holding a pipe cannot hang the run.
	waitDelay = 5 * time.Second
)

// LocalRuntime runs commands on the local host: it shallow-clones the repo into
// a temp directory, overlays the changed files, and executes the command with
// os/exec. It provides no isolation beyond a scratch directory, so it suits a
// trusted dev or CI host and a controlled demo; use a SandboxRuntime for
// untrusted execution at scale. Returns ErrUnavailable when git or the command
// binary is not on PATH, so a distroless deployment degrades to "skipped".
type LocalRuntime struct{}

// NewLocal returns a LocalRuntime.
func NewLocal() *LocalRuntime { return &LocalRuntime{} }

// Run materializes spec.Repo, overlays spec.Overlay, and executes spec.Command
// in the workspace root, returning the command's result. The workspace is
// removed before returning.
func (*LocalRuntime) Run(ctx context.Context, spec Spec) (Result, error) {
	if len(spec.Command) == 0 {
		return Result{}, fmt.Errorf("runtime: empty command")
	}
	if spec.Repo.Owner == "" || spec.Repo.Name == "" || spec.Repo.Ref == "" {
		return Result{}, fmt.Errorf("runtime: repo owner, name, and ref are required")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return Result{}, fmt.Errorf("%w: git not found", ErrUnavailable)
	}
	if _, err := exec.LookPath(spec.Command[0]); err != nil {
		return Result{}, fmt.Errorf("%w: %s not found", ErrUnavailable, spec.Command[0])
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "pad-verify-*")
	if err != nil {
		return Result{}, fmt.Errorf("runtime: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	if err := materialize(ctx, dir, spec.Repo); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return Result{TimedOut: true}, nil
		}
		return Result{}, err
	}
	if err := overlay(dir, spec.Overlay); err != nil {
		return Result{}, err
	}

	cmd := exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
	cmd.Dir = dir
	// Run in its own process group and kill the whole group on cancel, with a
	// bounded WaitDelay, so a build's descendants (compile, link, vet) cannot
	// outlive the deadline or hang on a held output pipe.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = waitDelay
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return os.ErrProcessDone
	}
	out, runErr := cmd.CombinedOutput()
	res := Result{Output: redactToken(tail(string(out), maxOutputBytes), spec.Repo.Token)}
	switch ctx.Err() {
	case context.DeadlineExceeded:
		res.TimedOut = true
		return res, nil
	case context.Canceled:
		// Parent cancellation is an infra event, not a fix defect: surface it so
		// the caller records "skipped" rather than "failed".
		return Result{}, ctx.Err()
	}
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("runtime: running %s: %w", spec.Command[0], runErr)
	}
	return res, nil
}

// materialize shallow-fetches repo.Ref into dir. Fetch by SHA works on GitHub,
// so a branch, tag, or commit all resolve the same way. A token authenticates
// the fetch via a one-shot http.extraheader so the credential is never written
// to the remote URL or .git/config, keeping it out of any later command output.
func materialize(ctx context.Context, dir string, repo RepoRef) error {
	url := fmt.Sprintf("https://github.com/%s/%s.git", repo.Owner, repo.Name)
	if repo.CloneURL != "" {
		url = repo.CloneURL
	}
	fetch := []string{"fetch", "-q", "--depth", "1", "origin", repo.Ref}
	if repo.Token != "" && repo.CloneURL == "" {
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + repo.Token))
		fetch = append([]string{"-c", "http.extraheader=AUTHORIZATION: Basic " + auth}, fetch...)
	}
	steps := [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", url},
		fetch,
		{"-c", "advice.detachedHead=false", "checkout", "-q", "FETCH_HEAD"},
	}
	for _, args := range steps {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		if err := cmd.Run(); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return ctx.Err()
			}
			// Redact the token from any echoed output before surfacing.
			return fmt.Errorf("runtime: git %s: %w: %s", args[0], err, redactToken(tail(buf.String(), 2048), repo.Token))
		}
	}
	return nil
}

// overlay writes each file over the checkout, rejecting paths that escape dir
// lexically or through a symlink in any existing parent component.
func overlay(dir string, files map[string]string) error {
	root := filepath.Clean(dir)
	for p, content := range files {
		clean := filepath.Clean("/" + p) // force absolute, drop any leading ../
		target := filepath.Join(root, clean)
		if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return fmt.Errorf("runtime: unsafe overlay path %q", p)
		}
		if err := noSymlinkParents(root, target); err != nil {
			return fmt.Errorf("runtime: overlay %s: %w", p, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("runtime: overlay %s: %w", p, err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return fmt.Errorf("runtime: overlay %s: %w", p, err)
		}
	}
	return nil
}

// noSymlinkParents rejects target if any existing path component between root
// and target is a symlink, so an overlay cannot be redirected outside the
// workspace through a symlinked directory in the checkout.
func noSymlinkParents(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	cur := root
	for _, part := range strings.Split(filepath.Dir(rel), string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil // remaining components will be created fresh
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinked parent %q", part)
		}
	}
	return nil
}

// tail returns the last max bytes of s, prefixed with an elision marker when
// truncated, so the useful end of a build log survives the bound.
func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "...(truncated)...\n" + s[len(s)-max:]
}

// redactToken removes the clone token from text before it is surfaced.
func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "REDACTED")
}
