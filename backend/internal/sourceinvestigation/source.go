// Package sourceinvestigation defines read-only source investigation contracts.
package sourceinvestigation

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/willie-yao/aster/backend/internal/actionverify"
	"github.com/willie-yao/aster/backend/internal/models"
	"github.com/willie-yao/aster/backend/internal/remediationpolicy"
)

const targetVerificationVersion = 1

var (
	// ErrUnavailable means the source runtime cannot run in this deployment.
	ErrUnavailable = errors.New("source investigation unavailable")
	// ErrInvalidResult means the agent returned an unsafe or malformed result.
	ErrInvalidResult = errors.New("invalid source investigation result")
)

const (
	RelationshipSupports     = "supports"
	RelationshipRefines      = "refines"
	RelationshipContradicts  = "contradicts"
	RelationshipInconclusive = "inconclusive"

	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"

	StateAlreadyPresent                = "already_present"
	StateActionableCodeChange          = "actionable_code_change"
	StateActionableConfigurationChange = "actionable_configuration_change"
	StateInconclusive                  = "inconclusive"
)

var fullCommitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

// Repository identifies the exact source checkout to investigate.
type Repository struct {
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	Revision string `json:"revision"`
}

// Citation identifies a bounded source range at the pinned revision.
type Citation struct {
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Quote     string `json:"quote"`
	Verified  bool   `json:"verified"`
}

// Result is the structured source investigation result.
type Result struct {
	State                     string                    `json:"state,omitempty"`
	Target                    *models.RemediationTarget `json:"target,omitempty"`
	Finding                   string                    `json:"finding"`
	Confidence                string                    `json:"confidence"`
	Relationship              string                    `json:"relationship"`
	Direction                 string                    `json:"direction"`
	Citations                 []Citation                `json:"citations,omitempty"`
	TargetVerificationVersion int                       `json:"target_verification_version,omitempty"`
	ElapsedMs                 int                       `json:"elapsed_ms,omitempty"`
}

// Reader reads one file from an exact repository revision.
type Reader interface {
	ReadFile(context.Context, Repository, string) (string, error)
}

// TreeReader lists the regular files at one exact repository revision.
type TreeReader interface {
	Reader
	ListFiles(context.Context, Repository) ([]string, error)
}

// ValidateRepository rejects mutable or ambiguous source revisions.
func ValidateRepository(repo Repository) error {
	repo.Owner = strings.TrimSpace(repo.Owner)
	repo.Name = strings.TrimSpace(repo.Name)
	repo.Revision = strings.TrimSpace(repo.Revision)
	if repo.Owner == "" || repo.Name == "" {
		return fmt.Errorf("%w: source repository owner and name are required", ErrUnavailable)
	}
	if !fullCommitPattern.MatchString(repo.Revision) {
		return fmt.Errorf("%w: source revision must be a full commit SHA", ErrUnavailable)
	}
	return nil
}

// ValidateResult bounds model-controlled output before persistence or rendering.
func ValidateResult(result Result) error {
	if strings.TrimSpace(result.Finding) == "" || len(result.Finding) > 8<<10 {
		return fmt.Errorf("%w: finding must be 1-%d bytes", ErrInvalidResult, 8<<10)
	}
	if strings.TrimSpace(result.Direction) == "" || len(result.Direction) > 4<<10 {
		return fmt.Errorf("%w: direction must be 1-%d bytes", ErrInvalidResult, 4<<10)
	}
	switch result.Confidence {
	case ConfidenceHigh, ConfidenceMedium, ConfidenceLow:
	default:
		return fmt.Errorf("%w: unsupported confidence %q", ErrInvalidResult, result.Confidence)
	}
	switch result.Relationship {
	case RelationshipSupports, RelationshipRefines, RelationshipContradicts, RelationshipInconclusive:
	default:
		return fmt.Errorf("%w: unsupported relationship %q", ErrInvalidResult, result.Relationship)
	}
	switch result.State {
	case "":
		// Legacy persisted results predate deterministic state classification.
	case StateInconclusive:
		if result.Target != nil {
			return fmt.Errorf("%w: inconclusive result must not claim a remediation target", ErrInvalidResult)
		}
	case StateAlreadyPresent, StateActionableCodeChange, StateActionableConfigurationChange:
		if result.Target == nil {
			return fmt.Errorf("%w: state %s requires a remediation target", ErrInvalidResult, result.State)
		}
		if reason := actionverify.PatternTargetReason(*result.Target); reason != "" {
			return fmt.Errorf("%w: %s", ErrInvalidResult, reason)
		}
		if result.State == StateActionableCodeChange && result.Target.Intent != models.RemediationIntentAddSymbol && result.Target.Intent != models.RemediationIntentModifySymbol {
			return fmt.Errorf("%w: actionable_code_change requires a symbol target", ErrInvalidResult)
		}
		if result.State == StateActionableConfigurationChange && result.Target.Intent != models.RemediationIntentSetConfiguration && result.Target.Intent != models.RemediationIntentRemoveConfiguration && result.Target.Intent != models.RemediationIntentSetJobEnvironment {
			return fmt.Errorf("%w: actionable_configuration_change requires a configuration target", ErrInvalidResult)
		}
	default:
		return fmt.Errorf("%w: unsupported state %q", ErrInvalidResult, result.State)
	}
	if result.Target != nil {
		if reason := remediationpolicy.Reason(result.Finding+"\n"+result.Direction, []models.RemediationTarget{*result.Target}); reason != "" {
			return fmt.Errorf("%w: remediation safety policy requires investigation", ErrInvalidResult)
		}
	}
	if err := ValidateCitations(result.Citations, 1, 10); err != nil {
		return err
	}
	totalBytes := len(result.Finding) + len(result.Direction)
	if result.Target != nil {
		totalBytes += len(result.Target.Intent) + len(result.Target.Path) + len(result.Target.Symbol) + len(result.Target.RequiredCall) + len(result.Target.Value) +
			len(result.Target.Repository) + len(result.Target.Revision) + len(result.Target.Job) + len(result.Target.Container) + len(result.Target.Name)
	}
	for _, citation := range result.Citations {
		totalBytes += len(citation.Path) + len(citation.Quote)
	}
	if result.Target != nil {
		matched := false
		for _, citation := range result.Citations {
			matched = matched || citation.Path == result.Target.Path
		}
		if !matched {
			return fmt.Errorf("%w: remediation target path is not cited", ErrInvalidResult)
		}
	}
	if totalBytes > 28<<10 {
		return fmt.Errorf("%w: result exceeds the persisted text budget", ErrInvalidResult)
	}
	return nil
}

// ValidateCitations bounds source citations before any repository read.
func ValidateCitations(citations []Citation, minCount, maxCount int) error {
	if minCount < 0 || maxCount < minCount || len(citations) < minCount || len(citations) > maxCount {
		return fmt.Errorf("%w: citations must contain %d-%d entries", ErrInvalidResult, minCount, maxCount)
	}
	seen := map[string]struct{}{}
	for i, citation := range citations {
		clean := path.Clean(strings.TrimSpace(citation.Path))
		if clean == "." || clean == ".." || clean != citation.Path || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, "\\") {
			return fmt.Errorf("%w: citation %d has unsafe path %q", ErrInvalidResult, i, citation.Path)
		}
		if citation.LineStart < 1 || citation.LineEnd < citation.LineStart || citation.LineEnd-citation.LineStart+1 > 200 {
			return fmt.Errorf("%w: citation %d has invalid line range", ErrInvalidResult, i)
		}
		if strings.TrimSpace(citation.Quote) == "" || len(citation.Quote) > 2<<10 {
			return fmt.Errorf("%w: citation %d quote must be 1-%d bytes", ErrInvalidResult, i, 2<<10)
		}
		key := fmt.Sprintf("%s:%d:%d:%s", citation.Path, citation.LineStart, citation.LineEnd, citation.Quote)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("%w: duplicate citation %d", ErrInvalidResult, i)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// VerifyCitations requires every citation quote to match the pinned source.
func VerifyCitations(ctx context.Context, reader Reader, repo Repository, citations []Citation) ([]Citation, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: source reader is not configured", ErrInvalidResult)
	}
	if err := ValidateRepository(repo); err != nil {
		return nil, err
	}
	if err := ValidateCitations(citations, 0, 10); err != nil {
		return nil, err
	}
	verified := slices.Clone(citations)
	cache := map[string]string{}
	for i := range verified {
		citation := &verified[i]
		content, ok := cache[citation.Path]
		if !ok {
			var err error
			content, err = reader.ReadFile(ctx, repo, citation.Path)
			if err != nil {
				return nil, fmt.Errorf("%w: reading cited source %q: %v", ErrInvalidResult, citation.Path, err)
			}
			cache[citation.Path] = content
		}
		lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
		if citation.LineStart < 1 || citation.LineEnd < citation.LineStart || citation.LineEnd > len(lines) {
			return nil, fmt.Errorf("%w: citation %q has an invalid line range", ErrInvalidResult, citation.Path)
		}
		selected := strings.Join(lines[citation.LineStart-1:citation.LineEnd], "\n")
		if !strings.Contains(selected, citation.Quote) {
			return nil, fmt.Errorf("%w: citation quote does not match %s:%d-%d", ErrInvalidResult, citation.Path, citation.LineStart, citation.LineEnd)
		}
		citation.Verified = true
	}
	return verified, nil
}

type repositoryBoundedSource struct {
	reader TreeReader
	repo   Repository
}

func (s repositoryBoundedSource) ListFiles(ctx context.Context) ([]string, error) {
	return s.reader.ListFiles(ctx, s.repo)
}

func (s repositoryBoundedSource) ReadFile(ctx context.Context, file string) (string, bool, error) {
	content, err := s.reader.ReadFile(ctx, s.repo, file)
	return content, err == nil, err
}

type targetVerificationReader struct {
	archive actionverify.Archive
	source  repositoryBoundedSource
}

func (r targetVerificationReader) ReadSourceArchive(context.Context) (actionverify.Archive, error) {
	return r.archive, nil
}

func (r targetVerificationReader) ReadFile(ctx context.Context, file string) (string, bool, error) {
	return r.source.ReadFile(ctx, file)
}

// VerifyTargetState deterministically checks one typed target at an immutable repository revision.
func VerifyTargetState(ctx context.Context, reader Reader, repo Repository, target models.RemediationTarget) (actionverify.Result, error) {
	tree, ok := reader.(TreeReader)
	if !ok {
		return actionverify.Result{}, fmt.Errorf("%w: bounded source tree is unavailable", ErrInvalidResult)
	}
	if err := ValidateRepository(repo); err != nil {
		return actionverify.Result{}, err
	}
	if target.Intent == models.RemediationIntentSetJobEnvironment {
		wantRepository := strings.TrimSpace(repo.Owner + "/" + repo.Name)
		if !strings.EqualFold(target.Repository, wantRepository) || !strings.EqualFold(target.Revision, repo.Revision) {
			return actionverify.Result{}, fmt.Errorf("%w: prow target source identity does not match the bounded repository", ErrInvalidResult)
		}
	}
	source := repositoryBoundedSource{reader: tree, repo: repo}
	archive, err := actionverify.BuildTargetArchive(ctx, source, []models.RemediationTarget{target})
	if err != nil {
		return actionverify.Result{}, fmt.Errorf("%w: remediation target source is unavailable", ErrInvalidResult)
	}
	verification, err := actionverify.Verify(ctx, targetVerificationReader{archive: archive, source: source}, actionverify.Input{Targets: []models.RemediationTarget{target}})
	if err != nil {
		return actionverify.Result{}, fmt.Errorf("%w: remediation target could not be verified", ErrInvalidResult)
	}
	return verification, nil
}

// VerifyResultTarget proves an actionable result against its pinned repository.
func VerifyResultTarget(ctx context.Context, reader Reader, repo Repository, result *Result) error {
	if result == nil || result.Target == nil {
		return nil
	}
	verification, err := VerifyTargetState(ctx, reader, repo, *result.Target)
	if err != nil {
		return err
	}
	want := actionverify.StateUnresolved
	if result.State == StateAlreadyPresent {
		want = actionverify.StateAlreadyPresent
	}
	if verification.State != want {
		return fmt.Errorf("%w: remediation target behavior is not proven", ErrInvalidResult)
	}
	result.TargetVerificationVersion = targetVerificationVersion
	return nil
}

// ValidateVerifiedResult requires every citation to match the pinned source.
func ValidateVerifiedResult(result Result) error {
	if err := ValidateResult(result); err != nil {
		return err
	}
	for i, citation := range result.Citations {
		if !citation.Verified {
			return fmt.Errorf("%w: citation %d is not verified", ErrInvalidResult, i)
		}
	}
	if result.Target != nil && result.TargetVerificationVersion != targetVerificationVersion {
		return fmt.Errorf("%w: remediation target behavior is not verified", ErrInvalidResult)
	}
	return nil
}

// CloneResult returns a detached result for owner-safe views.
func CloneResult(result *Result) *Result {
	if result == nil {
		return nil
	}
	out := *result
	out.Citations = slices.Clone(result.Citations)
	if result.Target != nil {
		target := *result.Target
		out.Target = &target
	}
	return &out
}
