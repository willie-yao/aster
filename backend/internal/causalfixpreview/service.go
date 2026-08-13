package causalfixpreview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actionverify"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/modelprovider"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/remediationinvestigation"
	engineruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

var (
	ErrInvalid       = errors.New("invalid causal fix preview request")
	ErrNotActionable = errors.New("causal remediation is not actionable")
	ErrRejected      = errors.New("causal fix patch was rejected")
	ErrValidation    = errors.New("causal fix validation failed")
	ErrConflict      = errors.New("causal fix preview idempotency conflict")
)

type Resolver interface {
	ResolveActionable(context.Context, remediationinvestigation.OperationRef) (remediationinvestigation.ActionableSubject, error)
}

type Options struct {
	Runtime          engineruntime.AgentRuntime
	ModelProvider    modelprovider.Config
	Timeout          time.Duration
	MaxSteps         int
	OutputLimitBytes int64
	RuntimeIdentity  string
	ApplyDiff        func(context.Context, engineruntime.RepoRef, string) (map[string]string, string, error)
}

type ValidationResult struct {
	Argv   []string `json:"argv"`
	Status string   `json:"status"`
	Output string   `json:"output,omitempty"`
}

type Preview struct {
	Summary         string                   `json:"summary"`
	BaseRevision    string                   `json:"base_revision"`
	Target          models.RemediationTarget `json:"target"`
	ChangedFiles    []string                 `json:"changed_files"`
	Diff            string                   `json:"diff"`
	Validations     []ValidationResult       `json:"validations"`
	RuntimeIdentity string                   `json:"runtime_identity,omitempty"`
}

type Service struct {
	resolver Resolver
	opts     Options
	mu       sync.Mutex
	seen     map[string]stored
}
type stored struct {
	subject, owner string
	preview        Preview
}

func New(resolver Resolver, opts Options) (*Service, error) {
	if resolver == nil || opts.Runtime == nil {
		return nil, fmt.Errorf("causal fix preview dependencies are required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Minute
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 20
	}
	if opts.OutputLimitBytes <= 0 {
		opts.OutputLimitBytes = 512 << 10
	}
	if opts.ApplyDiff == nil {
		opts.ApplyDiff = engineruntime.ApplyDiff
	}
	return &Service{resolver: resolver, opts: opts, seen: map[string]stored{}}, nil
}

func (s *Service) Preview(ctx context.Context, ref remediationinvestigation.OperationRef, owner, requestID string) (Preview, error) {
	owner, requestID = strings.TrimSpace(owner), strings.TrimSpace(requestID)
	if owner == "" || requestID == "" {
		return Preview{}, ErrInvalid
	}
	subject, err := s.resolver.ResolveActionable(ctx, ref)
	if errors.Is(err, remediationinvestigation.ErrOperationNotActionable) {
		return Preview{}, ErrNotActionable
	}
	if err != nil {
		return Preview{}, err
	}
	digest := subjectDigest(subject)
	key := owner + "\x00" + requestID
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior, ok := s.seen[key]; ok {
		if prior.subject != digest {
			return Preview{}, ErrConflict
		}
		return prior.preview, nil
	}
	preview, err := s.generate(ctx, subject)
	if err != nil {
		return Preview{}, err
	}
	s.seen[key] = stored{subject: digest, owner: owner, preview: preview}
	return preview, nil
}

func (s *Service) generate(ctx context.Context, subject remediationinvestigation.ActionableSubject) (Preview, error) {
	proposal := subject.Proposal
	switch proposal.TargetKind {
	case remediationinvestigation.TargetAddRequiredCall, remediationinvestigation.TargetSetJobEnvironment:
	default:
		return Preview{}, ErrNotActionable
	}
	if len(proposal.AllowedChangedPaths) == 0 || len(proposal.AllowedValidationCommands) == 0 {
		return Preview{}, ErrNotActionable
	}
	commands, err := executionCommands(proposal.AllowedValidationCommands, s.opts.Timeout)
	if err != nil {
		return Preview{}, err
	}
	prompt, _ := json.Marshal(struct {
		Target   any      `json:"target"`
		Expected string   `json:"expected_behavior"`
		Evidence any      `json:"selected_evidence"`
		Allowed  []string `json:"allowed_paths"`
	}{proposal.Target, proposal.ExpectedBehavior, subject.Evidence, proposal.AllowedChangedPaths})
	aiusage.MarkExternalUnmetered(ctx)
	result, err := s.opts.Runtime.Generate(ctx, engineruntime.GenerateSpec{
		Repo:            engineruntime.RepoRef{Owner: proposal.Repository.Owner, Name: proposal.Repository.Name, Ref: proposal.Repository.Revision},
		Instruction:     "Implement only this engine-verified target. Do not replace or broaden it. JSON data follows:\n" + string(prompt),
		ExpectedBaseSHA: proposal.Repository.Revision, MaxSteps: s.opts.MaxSteps, MaxFiles: len(proposal.AllowedChangedPaths), Timeout: s.opts.Timeout,
		ModelProvider: s.opts.ModelProvider, CommandPolicy: engineruntime.CommandPolicy{Commands: commands}, OutputLimitBytes: s.opts.OutputLimitBytes,
	})
	if err != nil {
		return Preview{}, fmt.Errorf("generation failed: %w", err)
	}
	if err := engineruntime.ValidateSuccessfulCommandResults(commands, result.CommandResults); err != nil {
		return Preview{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	current, err := s.resolver.ResolveActionable(ctx, remediationinvestigation.OperationRef{
		JobID: subject.Input.JobID, PatternID: subject.Input.PatternID, PatternHash: subject.Input.PatternHash,
		CausalGroupID: subject.Input.CausalGroupID, CausalGroupHash: subject.Input.CausalGroupHash,
	})
	if err != nil || subjectDigest(current) != subjectDigest(subject) {
		return Preview{}, ErrNotActionable
	}
	if len(result.Files) == 0 || strings.TrimSpace(result.Diff) == "" {
		return Preview{}, ErrRejected
	}
	reconstructed, canonicalDiff, err := s.opts.ApplyDiff(ctx, engineruntime.RepoRef{Owner: proposal.Repository.Owner, Name: proposal.Repository.Name, Ref: proposal.Repository.Revision}, result.Diff)
	if err != nil {
		return Preview{}, fmt.Errorf("%w: %v", ErrRejected, err)
	}
	if !sameFiles(result.Files, reconstructed) {
		return Preview{}, fmt.Errorf("%w: changed-file mismatch", ErrRejected)
	}
	allowed := map[string]bool{}
	for _, path := range proposal.AllowedChangedPaths {
		allowed[path] = true
	}
	paths := make([]string, 0, len(reconstructed))
	for path := range reconstructed {
		if !allowed[path] {
			return Preview{}, fmt.Errorf("%w: unexpected path", ErrRejected)
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	verification, err := sourceinvestigation.VerifyTargetState(ctx, overlayReader{base: subject.Source, files: reconstructed}, proposal.Repository, proposal.Target)
	if err != nil || verification.State != actionverify.StateAlreadyPresent {
		return Preview{}, fmt.Errorf("%w: target postcondition was not established: state=%s reason=%s err=%v", ErrRejected, verification.State, verification.Reason, err)
	}
	validations := make([]ValidationResult, 0, len(result.CommandResults))
	for _, command := range result.CommandResults {
		validations = append(validations, ValidationResult{Argv: slices.Clone(command.Argv), Status: "passed"})
	}
	return Preview{Summary: proposal.ExpectedBehavior, BaseRevision: proposal.Repository.Revision, Target: proposal.Target, ChangedFiles: paths, Diff: canonicalDiff, Validations: validations, RuntimeIdentity: s.opts.RuntimeIdentity}, nil
}

type overlayReader struct {
	base  sourceinvestigation.TreeReader
	files map[string]string
}

func (r overlayReader) ReadFile(ctx context.Context, repo sourceinvestigation.Repository, path string) (string, error) {
	if content, ok := r.files[path]; ok {
		return content, nil
	}
	return r.base.ReadFile(ctx, repo, path)
}
func (r overlayReader) ListFiles(ctx context.Context, repo sourceinvestigation.Repository) ([]string, error) {
	return r.base.ListFiles(ctx, repo)
}
func subjectDigest(subject remediationinvestigation.ActionableSubject) string {
	payload, _ := json.Marshal(struct {
		Input, Result, Evidence string
		Proposal                remediationinvestigation.ActionableProposal
	}{remediationinvestigation.FrozenInputDigest(subject.Input), subject.ResultDigest, subject.EvidenceCatalogDigest, subject.Proposal})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func executionCommands(commands []remediationinvestigation.ValidationCommand, overall time.Duration) ([]engineruntime.ExecutionCommand, error) {
	out := make([]engineruntime.ExecutionCommand, 0, len(commands)+1)
	for _, command := range commands {
		d, err := time.ParseDuration(command.Timeout)
		if err != nil {
			return nil, err
		}
		out = append(out, engineruntime.ExecutionCommand{Argv: slices.Clone(command.Argv), TimeoutSeconds: int64(d / time.Second)})
	}
	out = append(out, engineruntime.ExecutionCommand{Argv: []string{"git", "diff", "--cached", "--check"}, TimeoutSeconds: int64(overall / time.Second)})
	return out, nil
}
func sameFiles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
