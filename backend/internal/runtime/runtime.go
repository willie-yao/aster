// Package runtime is the swappable agent-execution abstraction for the engine.
// A Runtime materializes a workspace and runs a command in it, isolated from the
// caller. LocalRuntime (a temp clone plus os/exec) ships first and needs no
// cluster dependency; a SandboxRuntime backed by kubernetes-sigs/agent-sandbox
// can be added later behind the same interface for stronger isolation and
// per-session reuse.
//
// The first consumer is fix-PR verification: build and vet a proposed change
// against the real repository before the PR is opened. The batch fetcher stays
// plain first-party code and does not use a Runtime.
package runtime

import (
	"context"
	"errors"
	"time"
)

// ErrUnavailable reports that the runtime cannot execute in this environment,
// for example the required toolchain (git or the command binary) is not on
// PATH. Callers treat it as "verification skipped", never as a failure, so a
// distroless deployment without a toolchain degrades gracefully.
var ErrUnavailable = errors.New("runtime unavailable")

// RepoRef identifies a Git repository and the ref to materialize.
type RepoRef struct {
	Owner string
	Name  string
	// Ref is a branch, tag, or commit SHA to check out.
	Ref string
	// Token, when set, authenticates the clone of a private repo. Empty clones
	// anonymously, which is enough for a public repo.
	Token string
	// CloneURL overrides the derived https://github.com/<owner>/<name>.git URL,
	// for a mirror, an enterprise host, or a local path in tests. Optional.
	CloneURL string
}

// Spec is a single one-shot execution: materialize Repo at its ref, overlay the
// changed files, and run Command in the workspace root.
type Spec struct {
	Repo RepoRef
	// Overlay maps repo-relative path to full new file content, written over the
	// checkout before Command runs. This is the proposed fix's changed files.
	Overlay map[string]string
	// Command is the argv to run (Command[0] must resolve on PATH). Empty is an
	// error.
	Command []string
	// Timeout bounds the whole run (clone plus command). Zero uses a default.
	Timeout time.Duration
}

// Result is the outcome of a run. Passed reports a zero exit code.
type Result struct {
	ExitCode int
	// Output is the combined stdout and stderr, tail-truncated to a bound.
	Output   string
	TimedOut bool
}

// Passed reports whether the command exited zero and did not time out.
func (r Result) Passed() bool { return r.ExitCode == 0 && !r.TimedOut }

// Runtime materializes a workspace and runs one command in it. Implementations
// must isolate the workspace from the caller and tear it down before returning.
type Runtime interface {
	Run(ctx context.Context, spec Spec) (Result, error)
}
