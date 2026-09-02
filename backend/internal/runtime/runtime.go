// Package runtime defines execution contracts for coding-agent runtimes.
// Backends define their own process isolation and cleanup guarantees.
package runtime

import "errors"

// ErrUnavailable reports that the configured runtime cannot execute in this
// environment.
var ErrUnavailable = errors.New("runtime unavailable")

// ErrSandboxUnavailable reports that a required process sandbox cannot enforce
// its policy in the current environment. Callers must not run unsandboxed.
var ErrSandboxUnavailable = errors.New("runtime sandbox unavailable")

// ErrWorkIdentityChanged means a named external execution now has another UID.
var ErrWorkIdentityChanged = errors.New("runtime work identity changed")

// ErrCleanupPending means external runtime cleanup could not be confirmed yet.
var ErrCleanupPending = errors.New("runtime cleanup is still pending")

// ErrStaging means the external runtime could not materialize its sealed workspace.
var ErrStaging = errors.New("runtime staging failed")

// ErrMalformedResult means the external runtime returned an unreadable result envelope.
var ErrMalformedResult = errors.New("runtime result is malformed")

// ErrResultContract means the external runtime result violated its declared contract.
var ErrResultContract = errors.New("runtime result contract violation")

// ErrResultScope means the generated change exceeded the configured review scope.
var ErrResultScope = errors.New("runtime result exceeds review scope")

// ErrResultDeletion means the external runtime attempted to delete a repository file.
var ErrResultDeletion = errors.New("runtime result attempted deletion")

// ErrResultRename means the external runtime attempted to rename a repository file.
var ErrResultRename = errors.New("runtime result attempted rename")

// ErrResultExtraFile means the external runtime returned an unexpected file set.
var ErrResultExtraFile = errors.New("runtime result contains unexpected files")

// ErrCancelled means the external runtime reported a cancelled execution.
var ErrCancelled = errors.New("runtime execution cancelled")

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
