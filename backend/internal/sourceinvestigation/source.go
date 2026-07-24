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
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

var (
	// ErrUnavailable means the source runtime cannot run in this deployment.
	ErrUnavailable = errors.New("source investigation unavailable")
	// ErrInvalidResult means the agent returned an unsafe or malformed result.
	ErrInvalidResult = errors.New("invalid source investigation result")
)

const (
	StatusPending   = "pending"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusUnknown   = "unknown"

	PhaseQueued        = "queued"
	PhaseCloning       = "cloning_source"
	PhaseInvestigating = "investigating_source"
	PhaseVerifying     = "verifying_citations"
	PhaseFinalizing    = "finalizing"
	PhaseCancelling    = "cancelling"

	RelationshipSupports     = "supports"
	RelationshipRefines      = "refines"
	RelationshipContradicts  = "contradicts"
	RelationshipInconclusive = "inconclusive"

	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"
)

var fullCommitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

// Repository identifies the exact source checkout to investigate.
type Repository struct {
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	Revision string `json:"revision"`
}

// Subject is the immutable published-analysis and chat context for one run.
type Subject struct {
	SessionID           string           `json:"session_id"`
	ChatRequestID       string           `json:"chat_request_id"`
	Repository          Repository       `json:"repository"`
	JobID               string           `json:"job_id"`
	BuildPrefix         string           `json:"build_prefix"`
	Build               models.BuildInfo `json:"build"`
	TestCase            models.TestCase  `json:"test_case"`
	Question            string           `json:"question"`
	Answer              string           `json:"answer"`
	AnalysisGeneratedAt string           `json:"analysis_generated_at"`
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
	Finding      string     `json:"finding"`
	Confidence   string     `json:"confidence"`
	Relationship string     `json:"relationship"`
	Direction    string     `json:"direction"`
	Citations    []Citation `json:"citations,omitempty"`
	ElapsedMs    int        `json:"elapsed_ms,omitempty"`
}

// View is the owner-safe persisted request representation.
type View struct {
	ID            string  `json:"id"`
	SessionID     string  `json:"session_id"`
	ChatRequestID string  `json:"chat_request_id"`
	Status        string  `json:"status"`
	Phase         string  `json:"phase,omitempty"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
	ExpiresAt     string  `json:"expires_at"`
	Result        *Result `json:"result,omitempty"`
}

// Progress is a persisted, owner-safe investigation phase.
type Progress struct {
	RequestID string `json:"request_id"`
	Phase     string `json:"phase"`
	UpdatedAt string `json:"updated_at"`
}

// Request is one pinned source investigation run.
type Request struct {
	ID       string
	Subject  Subject
	Timeout  time.Duration
	Progress func(string)
}

// ReportProgress records a non-sensitive phase when configured.
func (r Request) ReportProgress(phase string) {
	if r.Progress != nil && ValidPhase(phase) {
		r.Progress(phase)
	}
}

// Runner investigates one exact source revision without modifying it.
type Runner interface {
	Investigate(context.Context, Request) (Result, error)
}

// ValidPhase reports whether phase is safe to expose to an owner.
func ValidPhase(phase string) bool {
	switch phase {
	case PhaseQueued, PhaseCloning, PhaseInvestigating, PhaseVerifying, PhaseFinalizing, PhaseCancelling:
		return true
	default:
		return false
	}
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
	if len(result.Citations) == 0 || len(result.Citations) > 10 {
		return fmt.Errorf("%w: citations must contain 1-10 entries", ErrInvalidResult)
	}
	totalBytes := len(result.Finding) + len(result.Direction)
	seen := map[string]struct{}{}
	for i, citation := range result.Citations {
		clean := path.Clean(strings.TrimSpace(citation.Path))
		if clean == "." || clean != citation.Path || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, "\\") {
			return fmt.Errorf("%w: citation %d has unsafe path %q", ErrInvalidResult, i, citation.Path)
		}
		if citation.LineStart < 1 || citation.LineEnd < citation.LineStart || citation.LineEnd-citation.LineStart+1 > 200 {
			return fmt.Errorf("%w: citation %d has invalid line range", ErrInvalidResult, i)
		}
		if strings.TrimSpace(citation.Quote) == "" || len(citation.Quote) > 2<<10 {
			return fmt.Errorf("%w: citation %d quote must be 1-%d bytes", ErrInvalidResult, i, 2<<10)
		}
		totalBytes += len(citation.Path) + len(citation.Quote)
		key := fmt.Sprintf("%s:%d:%d:%s", citation.Path, citation.LineStart, citation.LineEnd, citation.Quote)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("%w: duplicate citation %d", ErrInvalidResult, i)
		}
		seen[key] = struct{}{}
	}
	if totalBytes > 28<<10 {
		return fmt.Errorf("%w: result exceeds the persisted text budget", ErrInvalidResult)
	}
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
	return nil
}

// CloneResult returns a detached result for owner-safe views.
func CloneResult(result *Result) *Result {
	if result == nil {
		return nil
	}
	out := *result
	out.Citations = slices.Clone(result.Citations)
	return &out
}
