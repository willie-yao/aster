package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	gotoken "go/token"
	"io"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/tools/repotree"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/textutil"
)

// patternPromptVersion is bumped when the pattern prompt or output contract
// changes, so cached verdicts from an older contract are re-run.
const patternPromptVersion = 4

const patternCacheVersion = 2

// patternRepairVersion is included in pattern cache keys so repair contract
// changes invalidate verdicts produced by an older repair prompt.
const patternRepairVersion = 1

// maxPatternBuilds caps how many per-build analyses are fed into one pattern
// call, keeping the prompt bounded for a test that failed in many builds.
const maxPatternBuilds = 10

// patternMaxIters bounds the repotree tool rounds the grounded correlation loop
// spends verifying file and config paths against the real source repo.
const patternMaxIters = 6

const maxPatternResponseBytes = 1 << 20

// PatternFailure is one build's analyzed job failure, used as input to
// cross-failure correlation. FailingTest is the specific test or spec that
// failed in this build and may differ across builds.
type PatternFailure struct {
	BuildID        string
	FailingTest    string
	FailureMessage string
	RootCause      string
	// SuggestedFix is this build's per-failure remediation. The correlation
	// preserves the most specific one rather than regressing to the symptom.
	SuggestedFix string
	// RelevantFiles are the source files this build's analysis implicated,
	// so the correlation can name concrete targets in the cross-cutting fix.
	RelevantFiles []string
	// LocationFile is the failing test's own source file (from the JUnit
	// failure location). It seeds the fix harness but is deliberately kept out
	// of the correlation prompt so it never perturbs the pattern cache key.
	LocationFile string
	IsTransient  bool
	Severity     string
}

// PatternInput is the bounded, deterministic model input for one job-level
// correlation. Failures are newest-first and capped to the prompt limit.
type PatternInput struct {
	SystemPrompt string
	UserPrompt   string
	Failures     []PatternFailure
}

// patternResponse is the model's JSON contract for the correlation verdict.
type patternResponse struct {
	Systemic           bool                       `json:"systemic"`
	Confidence         string                     `json:"confidence"`
	SharedRootCause    string                     `json:"shared_root_cause"`
	SharedBuilds       []string                   `json:"shared_builds"`
	SuggestedFix       string                     `json:"suggested_fix"`
	RemediationTargets []models.RemediationTarget `json:"remediation_targets"`
	Summary            string                     `json:"summary"`
}

type patternCacheData struct {
	Version   int               `json:"version"`
	Response  patternResponse   `json:"response"`
	FileLinks map[string]string `json:"file_links,omitempty"`
	SourceRef string            `json:"source_ref,omitempty"`
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

type patternValidationCategory string

const (
	patternValidationJSON      patternValidationCategory = "json"
	patternValidationMissing   patternValidationCategory = "missing"
	patternValidationSchema    patternValidationCategory = "schema"
	patternValidationBuilds    patternValidationCategory = "builds"
	patternValidationAmbiguous patternValidationCategory = "ambiguous"
)

type patternParseStats struct {
	CandidateCount            int
	ValidCount                int
	UniqueValidCount          int
	IncompleteCount           int
	ContractLikeRejectedCount int
	ScanTruncated             bool
}

type patternValidationError struct {
	category patternValidationCategory
	issue    string
	stats    patternParseStats
}

// PatternRepairAttempt reports one bounded validation-repair completion.
type PatternRepairAttempt struct {
	Succeeded       bool
	FailureCategory PatternFailureCategory
}

// PatternAnalyzeOptions configure one full correlation attempt.
type PatternAnalyzeOptions struct {
	AllowAmbiguityRepair  bool
	AllowValidationRepair bool
	OnRepair              func(PatternRepairAttempt)
	OnCacheHit            func()
}

// PatternFailureCategory is a privacy-safe pattern-attempt outcome.
type PatternFailureCategory string

const (
	PatternFailureNone             PatternFailureCategory = ""
	PatternFailureJSON             PatternFailureCategory = "json"
	PatternFailureMissing          PatternFailureCategory = "missing"
	PatternFailureSchema           PatternFailureCategory = "schema"
	PatternFailureBuilds           PatternFailureCategory = "builds"
	PatternFailureAmbiguous        PatternFailureCategory = "ambiguous"
	PatternFailureRequestTimeout   PatternFailureCategory = "request-timeout"
	PatternFailureRateLimited      PatternFailureCategory = "rate-limited"
	PatternFailureProvider5xx      PatternFailureCategory = "provider-5xx"
	PatternFailureProvider         PatternFailureCategory = "provider"
	PatternFailureContextHeadroom  PatternFailureCategory = "context-headroom"
	PatternFailureToolsUnsupported PatternFailureCategory = "tools-unsupported"
	PatternFailureCancelled        PatternFailureCategory = "cancelled"
	PatternFailureDeadline         PatternFailureCategory = "deadline"
	PatternFailureUnknown          PatternFailureCategory = "unknown"
)

// PatternProviderError reports only a provider status class. It never includes
// the response body.
type PatternProviderError struct {
	StatusCode int
}

func (e *PatternProviderError) Error() string {
	return fmt.Sprintf("pattern analysis: provider request failed (%s)", patternProviderFailureCategory(e.StatusCode))
}

func (e *patternValidationError) Error() string {
	return fmt.Sprintf("pattern analysis: response validation failed (%s)", e.category)
}

func patternValidationCategoryOf(err error) patternValidationCategory {
	var validationErr *patternValidationError
	if errors.As(err, &validationErr) {
		return validationErr.category
	}
	return ""
}

func patternValidationIssueOf(err error) string {
	var validationErr *patternValidationError
	if errors.As(err, &validationErr) {
		return validationErr.issue
	}
	return ""
}

// PatternFailureCategoryOf classifies a pattern error without exposing model
// output, provider bodies, prompts, or private paths.
func PatternFailureCategoryOf(err error) PatternFailureCategory {
	if err == nil {
		return PatternFailureNone
	}
	if category := patternValidationCategoryOf(err); category != "" {
		return PatternFailureCategory(category)
	}
	var providerErr *PatternProviderError
	if errors.As(err, &providerErr) {
		return patternProviderFailureCategory(providerErr.StatusCode)
	}
	switch {
	case errors.Is(err, ErrContextHeadroom):
		return PatternFailureContextHeadroom
	case errors.Is(err, ErrToolsUnsupported):
		return PatternFailureToolsUnsupported
	case errors.Is(err, context.Canceled):
		return PatternFailureCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return PatternFailureDeadline
	default:
		return PatternFailureUnknown
	}
}

// IsRetryablePatternError reports whether one fresh correlation attempt is
// allowed after the first failed attempt.
func IsRetryablePatternError(err error) bool {
	switch PatternFailureCategoryOf(err) {
	case PatternFailureRequestTimeout, PatternFailureRateLimited, PatternFailureProvider5xx:
		return true
	default:
		return false
	}
}

func patternProviderFailureCategory(statusCode int) PatternFailureCategory {
	switch {
	case statusCode == http.StatusRequestTimeout:
		return PatternFailureRequestTimeout
	case statusCode == http.StatusTooManyRequests:
		return PatternFailureRateLimited
	case statusCode >= 500 && statusCode <= 599:
		return PatternFailureProvider5xx
	default:
		return PatternFailureProvider
	}
}

func safePatternProviderError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrContextHeadroom) || errors.Is(err, ErrToolsUnsupported) {
		return err
	}
	var patternErr *PatternProviderError
	if errors.As(err, &patternErr) {
		return patternErr
	}
	var httpErr *modelHTTPError
	if errors.As(err, &httpErr) {
		return &PatternProviderError{StatusCode: httpErr.StatusCode}
	}
	return &PatternProviderError{}
}

// patternSystemPrompt instructs the model to correlate several per-build
// analyses of the same job into one systemic-vs-transient verdict.
const patternSystemPrompt = `You correlate multiple failed builds of the SAME CI job to decide whether they share one underlying root cause.

You are given N independent per-build failure analyses from recent failed builds of one job. Each build was analyzed in isolation, so each may have called its own failure "transient". The specific test or spec that failed may differ from build to build. Each per-build analysis may also carry its own root_cause, suggested_fix, and the source files it implicated. Your job is the cross-build view those single analyses cannot have.

Key principle: a failure mode that recurs across most builds is NOT a flake, it is a systemic bug. "Transient" infrastructure errors (timeouts, resource exhaustion, slow disk, quota, image-pull) that appear in the majority of recent runs almost always have a fixable systemic cause (e.g. an undersized VM, a tight timeout, a missing image, a misconfigured template). Weigh the underlying MECHANISM, not the surface symptom: the same root cause can present as different-looking failures (different test flavors, different failing specs, different error strings).

Preserve signal, do not flatten it. The per-build analyses often already pinpoint the mechanism (a specific error, a named operation, an implicated file). Carry the MOST SPECIFIC evidence-backed cause and fix forward; do NOT regress to the lowest-common-denominator symptom that every build merely shares. If one build identified a concrete mechanism (e.g. "concurrent agent-pool update -> Azure OperationNotAllowed") and another only saw the symptom, the shared cause is the concrete mechanism, not the symptom.

Distinguish symptom from root cause. "VM bootstrapping failed", "test timed out", "node never joined" are SYMPTOMS. The root cause is WHY: the specific operation, condition, config, or code path that produced them.

The suggested_fix must be ACTIONABLE: name the specific change, the mechanism it addresses, and the component / file / config to change (cite a relevant_file when one is implicated). Do NOT emit non-fixes like "investigate the logs", "check why X fails", or "look into Y" - those are next steps, not fixes. If the evidence genuinely does not determine a concrete fix, say so plainly in suggested_fix AND lower confidence accordingly (do not claim high confidence on an undetermined fix).

Describe every proposed source change in remediation_targets. Use one object per target with exactly these fields:
- add_symbol: {"intent":"add_symbol","symbol":"PackageLevelGoIdentifier","path":"verified/repo/file.go"}
- modify_symbol: {"intent":"modify_symbol","symbol":"GoIdentifier","path":"verified/repo/file.go"}
- set_configuration: {"intent":"set_configuration","path":"verified/repo/file.yaml","value":"ExactKey=ExactValue"}
- remove_configuration: {"intent":"remove_configuration","path":"verified/repo/file.yaml","value":"ExactKey=ExactValue"}
- investigate: {"intent":"investigate"} only when the evidence does not identify an implementation-ready target
Do not claim verification state. The engine independently checks these targets against the pinned source.

Decide:
- systemic=true when most builds share one underlying cause. Name it precisely and give the concrete cross-cutting fix.
- systemic=false when the failures are genuinely unrelated or independently one-off.

Respond with ONLY a JSON object, no prose, no code fences:
{
  "systemic": true|false,
  "confidence": "high"|"medium"|"low",  // high only when the cause is specific AND the fix is concrete
  "shared_root_cause": "the one underlying MECHANISM (empty if not systemic); not a restated symptom",
  "shared_builds": ["buildID", ...],   // builds you judge to share the cause
  "suggested_fix": "the concrete, actionable cross-cutting fix naming the change and target (empty if not systemic)",
	"remediation_targets": [{"intent":"add_symbol|modify_symbol|set_configuration|remove_configuration|investigate","symbol":"optional","path":"optional","value":"optional"}],
  "summary": "one short paragraph: the verdict and the evidence for it"
}`

// patternGroundedSystemPrompt is patternSystemPrompt plus the repotree tool
// contract. It is used when a source-repo reader is wired, so the model
// verifies every file, template, or config path it names against the real
// repository instead of guessing one.
const patternGroundedSystemPrompt = patternSystemPrompt + `

You also have read-only tools over the source repository:
- list_repo_tree(path): immediate children of a directory
- read_repo_file(path, offset, len): byte-range read of one file
- grep_repo(pattern, path_glob?): RE2 search over a bounded file set

Ground every path you cite. BEFORE naming any file, template, manifest, or config in shared_root_cause, suggested_fix, or remediation_targets, confirm it exists by grepping for its name or the symbols/keys involved, listing the directory, and reading the file. NEVER invent or guess a path: an unread path is a hallucination. If you cannot find a real file that fits, use the investigate intent and lower confidence accordingly. When you are done investigating, respond with ONLY the JSON object described above and nothing else.`

func patternResponseFormat() ResponseFormat {
	stringProperty := func() map[string]any { return map[string]any{"type": "string"} }
	return ResponseFormat{
		Name:        "return_pattern",
		Description: "Return one validated recurring-pattern verdict.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"systemic":          map[string]any{"type": "boolean"},
				"confidence":        map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
				"shared_root_cause": stringProperty(),
				"shared_builds":     map[string]any{"type": "array", "items": stringProperty()},
				"suggested_fix":     stringProperty(),
				"remediation_targets": map[string]any{
					"type": "array", "maxItems": 8,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"intent": map[string]any{"type": "string", "enum": []string{
								models.RemediationIntentAddSymbol, models.RemediationIntentModifySymbol,
								models.RemediationIntentSetConfiguration, models.RemediationIntentRemoveConfiguration,
								models.RemediationIntentInvestigate,
							}},
							"symbol": stringProperty(), "path": stringProperty(), "value": stringProperty(),
						},
						"required": []string{"intent", "symbol", "path", "value"}, "additionalProperties": false,
					},
				},
				"summary": stringProperty(),
			},
			"required":             []string{"systemic", "confidence", "shared_root_cause", "shared_builds", "suggested_fix", "remediation_targets", "summary"},
			"additionalProperties": false,
		},
	}
}

// AnalyzePattern correlates the per-build analyses of one repeatedly-failing
// job into a single PatternAnalysis. It permits one bounded contract repair.
func (s *Service) AnalyzePattern(ctx context.Context, jobID, subject string, failures []PatternFailure) (*models.PatternAnalysis, error) {
	return s.AnalyzePatternWithOptions(ctx, jobID, subject, failures, PatternAnalyzeOptions{AllowAmbiguityRepair: true, AllowValidationRepair: true})
}

// AnalyzePatternWithOptions runs one full correlation attempt.
func (s *Service) AnalyzePatternWithOptions(ctx context.Context, jobID, subject string, failures []PatternFailure, options PatternAnalyzeOptions) (_ *models.PatternAnalysis, resultErr error) {
	var trace *TraceSession
	if s.traceStore != nil {
		trace = s.traceStore.Start(TraceMetadata{JobID: jobID, TestName: subject, APIMode: s.client.APIMode()})
		ctx = withAnalysisTrace(ctx, trace)
		defer func() {
			outcome := "pattern_success"
			if resultErr != nil {
				outcome = "pattern_error"
			}
			trace.Finish(outcome, resultErr)
		}()
	}

	input := BuildPatternInput(subject, failures)
	if len(input.Failures) < 2 {
		return nil, nil
	}
	failures = input.Failures
	usageOutcome := aiusage.OutcomeSuccess
	ctx, usageOperation := aiusage.Begin(ctx, s.usageRecorder, aiusage.Metadata{
		LogicalID: jobID + "\x00" + subject, Origin: s.usageOrigin,
		Feature: aiusage.FeaturePatternAnalysis, ModelFingerprint: s.client.modelFingerprint(),
		Correlation: aiusage.Correlation{JobID: jobID, TestName: subject},
	})
	defer func() {
		if errors.Is(resultErr, context.Canceled) || errors.Is(resultErr, context.DeadlineExceeded) {
			usageOutcome = aiusage.OutcomeCancelled
		} else if resultErr != nil {
			usageOutcome = aiusage.OutcomeError
		}
		usageOperation.Finish(usageOutcome)
	}()
	userPrompt := input.UserPrompt
	grounded := s.patternRepo != nil
	groundKey := "toolfree"
	if grounded {
		if resolver, ok := s.patternRepo.(interface{ ResolveRef(context.Context) error }); ok {
			if err := resolver.ResolveRef(ctx); err != nil {
				return nil, err
			}
		}
		groundKey = "grounded:" + s.sourceRepoOwner + "/" + s.sourceRepoName
		if identity, ok := s.patternRepo.(interface {
			SourceIdentity() (string, string, string)
		}); ok {
			_, _, ref := identity.SourceIdentity()
			groundKey += "@" + ref
		}
	}

	key := patternCacheKey(s.module.Name(), s.cacheGeneration, jobID, subject, userPrompt, groundKey, s.client.modelFingerprint())
	buildIDs := patternBuildIDs(failures)
	if raw, ok := s.client.cache.Get(key); ok {
		var cachedData patternCacheData
		if json.Unmarshal(raw, &cachedData) == nil && cachedData.Version == patternCacheVersion {
			if _, _, err := parsePatternResponseWithStats(string(mustJSON(cachedData.Response)), buildIDs); err == nil {
				if options.OnCacheHit != nil {
					options.OnCacheHit()
				}
				usageOutcome = aiusage.OutcomeCacheHit
				return buildPatternAnalysis(subject, len(failures), cachedData.Response, collectRelevantFiles(failures), cachedData.FileLinks, cachedData.SourceRef), nil
			}
		}
		if cached, stats, err := parsePatternResponseWithStats(string(raw), buildIDs); err == nil {
			if options.OnCacheHit != nil {
				options.OnCacheHit()
			}
			usageOutcome = aiusage.OutcomeCacheHit
			recordPatternParseTrace(ctx, "cache", stats, nil)
			return buildPatternAnalysis(subject, len(failures), cached, collectRelevantFiles(failures), nil, ""), nil
		}
	}

	var parsed patternResponse
	var sourceReads []string
	var err error
	if grounded {
		parsed, sourceReads, err = s.groundedPatternVerdict(ctx, userPrompt, buildIDs, options)
	} else {
		parsed, err = s.toolFreePatternVerdict(ctx, userPrompt, buildIDs, options)
	}
	if err != nil {
		return nil, err
	}

	var fileLinks map[string]string
	var sourceRef string
	if grounded {
		parsed.SuggestedFix = removeUnreadPatternPaths(parsed.SuggestedFix, sourceReads)
		if !patternTargetsWereRead(parsed.RemediationTargets, sourceReads) {
			return nil, &patternValidationError{category: patternValidationSchema}
		}
		fileLinks, sourceRef = s.patternFileLinks(parsed, sourceReads)
	} else {
		s.guardPatternPaths(ctx, &parsed)
		if !s.patternTargetsExist(ctx, parsed.RemediationTargets) {
			return nil, &patternValidationError{category: patternValidationSchema}
		}
	}
	_ = s.client.cache.Set(key, patternCacheData{Version: patternCacheVersion, Response: parsed, FileLinks: fileLinks, SourceRef: sourceRef})
	return buildPatternAnalysis(subject, len(failures), parsed, collectRelevantFiles(failures), fileLinks, sourceRef), nil
}

// BuildPatternInput renders the pattern-analysis contract.
func BuildPatternInput(subject string, failures []PatternFailure) PatternInput {
	prepared := append([]PatternFailure(nil), failures...)
	sort.Slice(prepared, func(i, j int) bool { return prepared[i].BuildID > prepared[j].BuildID })
	if len(prepared) > maxPatternBuilds {
		prepared = prepared[:maxPatternBuilds]
	}
	return PatternInput{
		SystemPrompt: patternSystemPrompt,
		UserPrompt:   buildPatternUserPrompt(subject, prepared),
		Failures:     prepared,
	}
}

// ParsePatternResult validates a model correlation result and converts it to
// the published PatternAnalysis shape.
func ParsePatternResult(subject string, failures []PatternFailure, result string) (*models.PatternAnalysis, error) {
	parsed, err := parsePatternResponse(result, patternBuildIDs(failures))
	if err != nil {
		return nil, err
	}
	// The Orka correlation task has no source-repository tools. Mark every path
	// it introduces as unverified rather than presenting a guessed target as fact.
	parsed.SuggestedFix = annotateUnverifiedPaths(parsed.SuggestedFix, func(string) bool { return false })
	return buildPatternAnalysis(subject, len(failures), parsed, collectRelevantFiles(failures), nil, ""), nil
}

type trackingPatternRepo struct {
	inner    tools.RepoReader
	reads    map[string]bool
	listTree func(context.Context) ([]string, error)
}

func (r *trackingPatternRepo) ListTree(ctx context.Context) ([]string, error) {
	if r.listTree != nil {
		return r.listTree(ctx)
	}
	return r.inner.ListTree(ctx)
}
func (r *trackingPatternRepo) ReadFile(ctx context.Context, path string) (string, bool, error) {
	content, found, err := r.inner.ReadFile(ctx, path)
	if err == nil && found {
		r.reads[path] = true
	}
	return content, found, err
}
func (r *trackingPatternRepo) Paths() []string {
	paths := make([]string, 0, len(r.reads))
	for p := range r.reads {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

func removeUnreadPatternPaths(text string, reads []string) string {
	read := map[string]bool{}
	for _, p := range reads {
		read[p] = true
	}
	return patternPathRe.ReplaceAllStringFunc(text, func(match string) string {
		if read[strings.TrimPrefix(match, "./")] {
			return match
		}
		return "the verified source location"
	})
}

func (s *Service) patternFileLinks(p patternResponse, reads []string) (map[string]string, string) {
	links := map[string]string{}
	owner, repo, ref := s.sourceRepoOwner, s.sourceRepoName, "HEAD"
	if identity, ok := s.patternRepo.(interface {
		SourceIdentity() (string, string, string)
	}); ok {
		owner, repo, ref = identity.SourceIdentity()
	}
	read := map[string]bool{}
	for _, path := range reads {
		read[path] = true
	}
	text := p.SharedRootCause + "\n" + p.SuggestedFix + "\n" + p.Summary
	for _, match := range patternPathRe.FindAllString(text, -1) {
		clean := strings.TrimPrefix(match, "./")
		if read[clean] {
			links[clean] = blobURLAtRef(owner, repo, ref, clean)
		}
	}
	for _, target := range p.RemediationTargets {
		if target.Path != "" && read[target.Path] {
			links[target.Path] = blobURLAtRef(owner, repo, ref, target.Path)
		}
	}
	if len(links) == 0 {
		return nil, owner + "/" + repo + "@" + ref
	}
	return links, owner + "/" + repo + "@" + ref
}

func patternTargetsWereRead(targets []models.RemediationTarget, reads []string) bool {
	read := make(map[string]bool, len(reads))
	for _, value := range reads {
		read[value] = true
	}
	for _, target := range targets {
		if target.Path != "" && !read[target.Path] {
			return false
		}
	}
	return true
}

func (s *Service) patternTargetsExist(ctx context.Context, targets []models.RemediationTarget) bool {
	exists := s.patternPathVerifier(ctx)
	if exists == nil {
		for _, target := range targets {
			if target.Path != "" {
				return false
			}
		}
		return true
	}
	for _, target := range targets {
		if target.Path != "" && !exists(target.Path) {
			return false
		}
	}
	return true
}

// toolFreePatternVerdict makes one correlation call without tools.
func (s *Service) toolFreePatternVerdict(ctx context.Context, userPrompt string, buildIDs map[string]struct{}, options PatternAnalyzeOptions) (patternResponse, error) {
	started := time.Now()
	messages := []modelMessage{
		{Role: "system", Content: strPtr(patternSystemPrompt)},
		{Role: "user", Content: strPtr(userPrompt)},
	}
	resp, err := s.client.callModel(ctx, messages, nil, nil)
	if err != nil {
		err = safePatternProviderError(err)
		recordTrace(ctx, TraceEvent{Kind: "pattern_request", Status: "tool_free", Outcome: "error", DurationMs: int(time.Since(started) / time.Millisecond), ErrorCode: patternTraceErrorCode(err)})
		return patternResponse{}, err
	}
	recordTrace(ctx, TraceEvent{Kind: "pattern_request", Status: "tool_free", Outcome: "success", DurationMs: int(time.Since(started) / time.Millisecond)})
	if !resp.HasMessage || resp.Message.Content == nil {
		return patternResponse{}, &patternValidationError{category: patternValidationMissing}
	}
	return s.parsePatternOutput(ctx, "tool_free", *resp.Message.Content, buildIDs, options)
}

// groundedPatternVerdict runs one correlation as a repotree tool loop.
func (s *Service) groundedPatternVerdict(ctx context.Context, userPrompt string, buildIDs map[string]struct{}, options PatternAnalyzeOptions) (patternResponse, []string, error) {
	reg := tools.NewRegistry()
	repotree.Register(reg)
	enabled, err := reg.Enable([]string{repotree.Group})
	if err != nil {
		return patternResponse{}, nil, fmt.Errorf("enabling repo tools: %w", err)
	}
	tracker := &trackingPatternRepo{inner: s.patternRepo, reads: map[string]bool{}, listTree: s.patternRepoTree}
	env := &tools.Env{Repo: tracker, Cache: tools.NewBoundedCache(128, 16<<20)}
	started := time.Now()
	out, err := s.client.ToolLoop(ctx, patternGroundedSystemPrompt, userPrompt, reg, enabled, env,
		ToolLoopOptions{MaxIters: patternMaxIters, MinToolCalls: 1, SingleToolCall: true, PropagateFinalizeError: true})
	if err != nil {
		err = safePatternProviderError(err)
		recordTrace(ctx, TraceEvent{Kind: "pattern_request", Status: "grounded", Outcome: "error", DurationMs: int(time.Since(started) / time.Millisecond), ErrorCode: patternTraceErrorCode(err)})
		return patternResponse{}, nil, err
	}
	recordTrace(ctx, TraceEvent{Kind: "pattern_request", Status: "grounded", Outcome: "success", DurationMs: int(time.Since(started) / time.Millisecond)})
	if strings.TrimSpace(out) == "" {
		return patternResponse{}, nil, &patternValidationError{category: patternValidationMissing}
	}
	parsed, perr := s.parsePatternOutput(ctx, "grounded", out, buildIDs, options)
	if perr == nil || patternValidationCategoryOf(perr) != patternValidationMissing {
		return parsed, tracker.Paths(), perr
	}

	extract := "Extract the correlation verdict this investigation reached as JSON with exactly these keys: " +
		`{"systemic": true|false, "confidence": "high"|"medium"|"low", "shared_root_cause": "...", "shared_builds": ["..."], "suggested_fix": "...", "remediation_targets": [{"intent":"...","symbol":"...","path":"...","value":"..."}], "summary": "..."}. ` +
		"Reply with ONLY the JSON.\n\nInvestigation:\n" + out
	started = time.Now()
	out2, err := s.client.Complete(ctx, "You output only a JSON object.", extract)
	if err != nil {
		err = safePatternProviderError(err)
		recordTrace(ctx, TraceEvent{Kind: "pattern_request", Status: "extraction", Outcome: "error", DurationMs: int(time.Since(started) / time.Millisecond), ErrorCode: patternTraceErrorCode(err)})
		return patternResponse{}, nil, err
	}
	recordTrace(ctx, TraceEvent{Kind: "pattern_request", Status: "extraction", Outcome: "success", DurationMs: int(time.Since(started) / time.Millisecond)})
	parsed, err = s.parsePatternOutput(ctx, "extraction", out2, buildIDs, options)
	return parsed, tracker.Paths(), err
}

func (s *Service) parsePatternOutput(ctx context.Context, stage, output string, buildIDs map[string]struct{}, options PatternAnalyzeOptions) (patternResponse, error) {
	parsed, stats, err := parsePatternResponseWithStats(output, buildIDs)
	recordPatternParseTrace(ctx, stage, stats, err)
	if err == nil || stats.ScanTruncated {
		return parsed, err
	}
	switch patternValidationCategoryOf(err) {
	case patternValidationAmbiguous:
		if options.AllowAmbiguityRepair {
			return s.repairPatternAmbiguity(ctx, output, buildIDs, options.OnRepair)
		}
	case patternValidationSchema, patternValidationBuilds:
		if options.AllowValidationRepair && len(output) <= int(defaultStructuredResponseBytes) {
			return s.repairPatternValidation(ctx, output, buildIDs, err, options.OnRepair)
		}
	}
	return parsed, err
}

func (s *Service) repairPatternValidation(ctx context.Context, output string, buildIDs map[string]struct{}, validationErr error, observe func(PatternRepairAttempt)) (patternResponse, error) {
	category := patternValidationCategoryOf(validationErr)
	issue := patternValidationIssueOf(validationErr)
	if issue == "" {
		issue = "contract_validation"
	}
	prompt := fmt.Sprintf("Repair contract version %d. The prior bounded investigation produced a recurring-pattern contract that failed deterministic validation. Validation category: %s. Validation issue: %s. Correct the contract without weakening or omitting any required field. Do not add an explanation or code fence. Return the corrected contract.\n\nPrior output:\n%s", patternRepairVersion, category, issue, output)
	started := time.Now()
	var parsed patternResponse
	var acceptedStats patternParseStats
	var parseStats patternParseStats
	var parseErr error
	validate := func(raw json.RawMessage) error {
		candidate, stats, err := parsePatternResponseWithStats(string(raw), buildIDs)
		if err != nil {
			parseStats, parseErr = stats, err
			return err
		}
		parsed, acceptedStats = candidate, stats
		return nil
	}
	err := s.client.CompleteStructured(ctx, "Return exactly one valid recurring-pattern contract.", prompt, patternResponseFormat(), validate)
	if err != nil {
		var structuredErr *structuredCompletionError
		validationOnly := errors.As(err, &structuredErr) && structuredErr.cause == nil
		if parseErr != nil && validationOnly {
			err = parseErr
			recordPatternParseTrace(ctx, "repair", parseStats, err)
		} else {
			err = safePatternProviderError(err)
		}
		recordTrace(ctx, TraceEvent{Kind: "pattern_repair", Status: "validation", Outcome: "rejected", DurationMs: int(time.Since(started) / time.Millisecond), ErrorCode: patternTraceErrorCode(err), ValidationCode: patternValidationIssueOf(err)})
		if observe != nil {
			observe(PatternRepairAttempt{Succeeded: false, FailureCategory: PatternFailureCategoryOf(err)})
		}
		return patternResponse{}, err
	}
	recordPatternParseTrace(ctx, "repair", acceptedStats, nil)
	recordTrace(ctx, TraceEvent{Kind: "pattern_repair", Status: "validation", Outcome: "success", DurationMs: int(time.Since(started) / time.Millisecond)})
	if observe != nil {
		observe(PatternRepairAttempt{Succeeded: true})
	}
	return parsed, nil
}

func (s *Service) repairPatternAmbiguity(ctx context.Context, output string, buildIDs map[string]struct{}, observe func(PatternRepairAttempt)) (patternResponse, error) {
	prompt := fmt.Sprintf("Repair contract version %d. The prior bounded investigation produced multiple distinct candidate contracts. Resolve them into exactly one verdict supported by that investigation. Do not add a draft, alternative, explanation, or code fence. Reply with only one JSON object using exactly these keys: "+
		`{"systemic": true|false, "confidence": "high"|"medium"|"low", "shared_root_cause": "...", "shared_builds": ["..."], "suggested_fix": "...", "remediation_targets": [{"intent":"...","symbol":"...","path":"...","value":"..."}], "summary": "..."}.`+"\n\nInvestigation output:\n%s", patternRepairVersion, output)
	started := time.Now()
	repaired, err := s.client.Complete(ctx, "Return exactly one final recurring-pattern JSON contract and no other content.", prompt)
	if err != nil {
		err = safePatternProviderError(err)
		recordTrace(ctx, TraceEvent{Kind: "pattern_repair", Status: "ambiguity", Outcome: "error", DurationMs: int(time.Since(started) / time.Millisecond), ErrorCode: patternTraceErrorCode(err)})
		if observe != nil {
			observe(PatternRepairAttempt{FailureCategory: PatternFailureCategoryOf(err)})
		}
		return patternResponse{}, err
	}
	parsed, stats, err := parsePatternResponseWithStats(repaired, buildIDs)
	recordPatternParseTrace(ctx, "repair", stats, err)
	outcome := "success"
	if err != nil {
		outcome = "validation_error"
	}
	recordTrace(ctx, TraceEvent{Kind: "pattern_repair", Status: "ambiguity", Outcome: outcome, DurationMs: int(time.Since(started) / time.Millisecond), ErrorCode: patternTraceErrorCode(err)})
	if observe != nil {
		observe(PatternRepairAttempt{Succeeded: err == nil, FailureCategory: PatternFailureCategoryOf(err)})
	}
	return parsed, err
}

func patternTraceErrorCode(err error) string {
	category := PatternFailureCategoryOf(err)
	if category == PatternFailureNone {
		return ""
	}
	return strings.ReplaceAll(string(category), "-", "_")
}

func recordPatternParseTrace(ctx context.Context, stage string, stats patternParseStats, err error) {
	outcome := "accepted"
	if err != nil {
		outcome = "rejected"
	}
	recordTrace(ctx, TraceEvent{
		Kind: "pattern_parse", Status: stage, Outcome: outcome,
		CandidateCount: stats.CandidateCount, ValidCount: stats.ValidCount,
		UniqueCandidateCount: stats.UniqueValidCount, IncompleteCount: stats.IncompleteCount,
		ContractLikeRejectedCount: stats.ContractLikeRejectedCount, ScanTruncated: stats.ScanTruncated,
		ErrorCode: patternTraceErrorCode(err), ValidationCode: patternValidationIssueOf(err),
	})
}

func parsePatternResponse(raw string, buildIDs map[string]struct{}) (patternResponse, error) {
	parsed, _, err := parsePatternResponseWithStats(raw, buildIDs)
	return parsed, err
}

func parsePatternResponseWithStats(raw string, buildIDs map[string]struct{}) (patternResponse, patternParseStats, error) {
	stats := patternParseStats{}
	validationError := func(category patternValidationCategory, issue string) (patternResponse, patternParseStats, error) {
		return patternResponse{}, stats, &patternValidationError{category: category, issue: issue, stats: stats}
	}
	if len(raw) > maxPatternResponseBytes {
		return validationError(patternValidationJSON, "response_too_large")
	}
	scan := scanAnalysisChatJSONCandidates(raw)
	stats.CandidateCount = len(scan.candidates)
	stats.IncompleteCount = len(scan.incomplete)
	stats.ScanTruncated = scan.truncated
	if scan.truncated {
		return validationError(patternValidationAmbiguous, "candidate_scan_truncated")
	}
	type validCandidate struct {
		response patternResponse
		start    int
		end      int
	}
	type rejectedCandidate struct {
		start        int
		end          int
		category     patternValidationCategory
		issue        string
		contractLike bool
	}
	valid := make([]validCandidate, 0, 1)
	rejected := make([]rejectedCandidate, 0, len(scan.candidates))
	contractLikeSeen := false
	bestCategory := patternValidationJSON
	bestIssue := "invalid_json"
	unique := map[string]patternResponse{}
	for _, candidate := range scan.candidates {
		parsed, category, issue := decodePatternCandidate(candidate.value, buildIDs)
		if category == "" {
			parsed = canonicalizePatternResponse(parsed)
			canonical, err := json.Marshal(parsed)
			if err != nil {
				return validationError(patternValidationJSON, "canonical_json")
			}
			key := string(canonical)
			valid = append(valid, validCandidate{response: parsed, start: candidate.start, end: candidate.end})
			unique[key] = parsed
			continue
		}
		contractLike := patternCandidateIsContractLike(candidate.value)
		rejected = append(rejected, rejectedCandidate{
			start: candidate.start, end: candidate.end, category: category, issue: issue, contractLike: contractLike,
		})
		if contractLike {
			stats.ContractLikeRejectedCount++
		}
		contractLikeSeen = contractLikeSeen || contractLike
		if patternValidationRank(category) > patternValidationRank(bestCategory) {
			bestCategory = category
			bestIssue = issue
		}
	}
	stats.ValidCount = len(valid)
	stats.UniqueValidCount = len(unique)
	if len(valid) == 0 {
		if !contractLikeSeen && len(scan.incomplete) == 0 {
			return validationError(patternValidationMissing, "no_contract")
		}
		return validationError(bestCategory, bestIssue)
	}

	firstStart, firstEnd, lastEnd := valid[0].start, valid[0].end, valid[0].end
	for _, candidate := range valid[1:] {
		if candidate.start < firstStart {
			firstStart, firstEnd = candidate.start, candidate.end
		}
		if candidate.end > lastEnd {
			lastEnd = candidate.end
		}
	}
	for _, incomplete := range scan.incomplete {
		if incomplete.start > firstEnd {
			return validationError(patternValidationJSON, "trailing_incomplete_json")
		}
	}
	for _, candidate := range rejected {
		if candidate.contractLike && (candidate.start > firstEnd ||
			(candidate.start < firstStart && candidate.end > lastEnd)) {
			return validationError(candidate.category, candidate.issue)
		}
	}
	if len(unique) != 1 {
		return validationError(patternValidationAmbiguous, "conflicting_valid_contracts")
	}
	for _, parsed := range unique {
		return parsed, stats, nil
	}
	panic("unreachable canonical pattern response")
}

func canonicalizePatternResponse(parsed patternResponse) patternResponse {
	parsed.Confidence = strings.ToLower(strings.TrimSpace(parsed.Confidence))
	parsed.SharedRootCause = strings.TrimSpace(parsed.SharedRootCause)
	parsed.SuggestedFix = strings.TrimSpace(parsed.SuggestedFix)
	parsed.Summary = strings.TrimSpace(parsed.Summary)
	builds := make([]string, 0, len(parsed.SharedBuilds))
	seen := make(map[string]struct{}, len(parsed.SharedBuilds))
	for _, buildID := range parsed.SharedBuilds {
		buildID = strings.TrimSpace(buildID)
		if _, ok := seen[buildID]; ok {
			continue
		}
		seen[buildID] = struct{}{}
		builds = append(builds, buildID)
	}
	sort.Strings(builds)
	parsed.SharedBuilds = builds
	for i := range parsed.RemediationTargets {
		parsed.RemediationTargets[i].Intent = strings.ToLower(strings.TrimSpace(parsed.RemediationTargets[i].Intent))
		parsed.RemediationTargets[i].Symbol = strings.TrimSpace(parsed.RemediationTargets[i].Symbol)
		parsed.RemediationTargets[i].Path = strings.TrimPrefix(strings.TrimSpace(parsed.RemediationTargets[i].Path), "./")
		parsed.RemediationTargets[i].Value = strings.TrimSpace(parsed.RemediationTargets[i].Value)
	}
	return parsed
}

func decodePatternCandidate(raw string, buildIDs map[string]struct{}) (patternResponse, patternValidationCategory, string) {
	fields, category, issue := decodePatternObject(raw)
	if category != "" {
		return patternResponse{}, category, issue
	}
	required := []string{"systemic", "confidence", "shared_root_cause", "shared_builds", "suggested_fix", "remediation_targets", "summary"}
	if len(fields) != len(required) {
		return patternResponse{}, patternValidationSchema, "required_fields"
	}
	for _, field := range required {
		value, ok := fields[field]
		if !ok || strings.TrimSpace(string(value)) == "null" {
			return patternResponse{}, patternValidationSchema, "required_fields"
		}
	}
	var parsed patternResponse
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return patternResponse{}, patternValidationSchema, "field_types"
	}
	if !decodeRemediationTargets(fields["remediation_targets"], &parsed.RemediationTargets) {
		return patternResponse{}, patternValidationSchema, "remediation_targets"
	}
	parsed = canonicalizePatternResponse(parsed)
	if category, issue := patternResponseValidation(parsed, buildIDs); category != "" {
		return patternResponse{}, category, issue
	}
	return parsed, "", ""
}

func decodePatternObject(raw string) (map[string]json.RawMessage, patternValidationCategory, string) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, patternValidationJSON, "invalid_json"
	}
	fields := make(map[string]json.RawMessage, 7)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, patternValidationJSON, "invalid_json"
		}
		name, ok := token.(string)
		if !ok {
			return nil, patternValidationJSON, "invalid_json"
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, patternValidationSchema, "duplicate_field"
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, patternValidationJSON, "invalid_json"
		}
		fields[name] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, patternValidationJSON, "invalid_json"
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, patternValidationJSON, "trailing_json"
	}
	return fields, "", ""
}

func patternCandidateIsContractLike(raw string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) == nil {
		for _, field := range []string{"systemic", "confidence", "shared_root_cause", "shared_builds", "suggested_fix", "remediation_targets", "summary"} {
			if _, ok := fields[field]; ok {
				return true
			}
		}
		return false
	}
	for _, field := range []string{"systemic", "confidence", "shared_root_cause", "shared_builds", "suggested_fix", "remediation_targets", "summary"} {
		if strings.Contains(raw, `"`+field+`"`) {
			return true
		}
	}
	return false
}

func patternResponseValidation(p patternResponse, buildIDs map[string]struct{}) (patternValidationCategory, string) {
	confidence := strings.ToLower(strings.TrimSpace(p.Confidence))
	if confidence != "high" && confidence != "medium" && confidence != "low" {
		return patternValidationSchema, "confidence"
	}
	if strings.TrimSpace(p.Summary) == "" {
		return patternValidationSchema, "summary"
	}
	if p.Systemic {
		if strings.TrimSpace(p.SharedRootCause) == "" || strings.TrimSpace(p.SuggestedFix) == "" || len(p.SharedBuilds) < 2 || len(p.RemediationTargets) == 0 {
			return patternValidationSchema, "systemic_contract"
		}
	} else if strings.TrimSpace(p.SharedRootCause) != "" || strings.TrimSpace(p.SuggestedFix) != "" || len(p.RemediationTargets) != 0 {
		return patternValidationSchema, "non_systemic_contract"
	}
	if len(p.RemediationTargets) > 8 {
		return patternValidationSchema, "remediation_target_limit"
	}
	for _, target := range p.RemediationTargets {
		if !validRemediationTarget(target) {
			return patternValidationSchema, "remediation_target"
		}
	}
	for _, buildID := range p.SharedBuilds {
		buildID = strings.TrimSpace(buildID)
		if buildID == "" {
			return patternValidationBuilds, "shared_builds"
		}
		if buildIDs != nil {
			if _, ok := buildIDs[buildID]; !ok {
				return patternValidationBuilds, "shared_builds"
			}
		}
	}
	return "", ""
}

func decodeRemediationTargets(raw json.RawMessage, targets *[]models.RemediationTarget) bool {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return false
	}
	parsed := make([]models.RemediationTarget, 0)
	for decoder.More() {
		var item json.RawMessage
		if err := decoder.Decode(&item); err != nil {
			return false
		}
		fields, category, _ := decodePatternObject(string(item))
		if category != "" || len(fields) == 0 || len(fields) > 4 {
			return false
		}
		for name, value := range fields {
			if name != "intent" && name != "symbol" && name != "path" && name != "value" {
				return false
			}
			if strings.TrimSpace(string(value)) == "null" {
				return false
			}
		}
		if _, ok := fields["intent"]; !ok {
			return false
		}
		var target models.RemediationTarget
		if err := json.Unmarshal(item, &target); err != nil {
			return false
		}
		parsed = append(parsed, target)
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		return false
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return false
	}
	*targets = parsed
	return true
}

func validRemediationTarget(target models.RemediationTarget) bool {
	validPath := func(value string) bool {
		if value == "" || len(value) > 512 || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
			return false
		}
		clean := path.Clean(value)
		return clean == value && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
	}
	validValue := func(value string) bool {
		key, expected, ok := strings.Cut(value, "=")
		return ok && strings.TrimSpace(key) != "" && strings.TrimSpace(expected) != "" &&
			len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00")
	}
	switch target.Intent {
	case models.RemediationIntentAddSymbol, models.RemediationIntentModifySymbol:
		return gotoken.IsIdentifier(target.Symbol) && validPath(target.Path) && target.Value == ""
	case models.RemediationIntentSetConfiguration, models.RemediationIntentRemoveConfiguration:
		return target.Symbol == "" && validPath(target.Path) && validValue(target.Value)
	case models.RemediationIntentInvestigate:
		return target.Symbol == "" && target.Path == "" && target.Value == ""
	default:
		return false
	}
}

func patternBuildIDs(failures []PatternFailure) map[string]struct{} {
	ids := make(map[string]struct{}, len(failures))
	for _, failure := range failures {
		if id := strings.TrimSpace(failure.BuildID); id != "" {
			ids[id] = struct{}{}
		}
	}
	return ids
}

func patternValidationRank(category patternValidationCategory) int {
	switch category {
	case patternValidationBuilds:
		return 3
	case patternValidationSchema:
		return 2
	case patternValidationJSON:
		return 1
	default:
		return 0
	}
}

// collectRelevantFiles unions the files each build implicated, in first-seen
// order, leading with the failing test's own source file. These carry the
// analysis's own targeting into the pattern so the fix harness can ground
// candidate selection on them rather than re-deriving targets from scratch.
func collectRelevantFiles(failures []PatternFailure) []string {
	seen := map[string]bool{}
	var out []string
	add := func(f string) {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			return
		}
		seen[f] = true
		out = append(out, f)
	}
	for _, f := range failures {
		add(f.LocationFile)
		for _, rf := range f.RelevantFiles {
			add(rf)
		}
	}
	return out
}

// buildPatternAnalysis converts a parsed verdict into the published model.
func buildPatternAnalysis(subject string, builds int, p patternResponse, relevantFiles []string, fileLinks map[string]string, sourceRef string) *models.PatternAnalysis {
	conf := strings.ToLower(strings.TrimSpace(p.Confidence))
	switch conf {
	case "high", "medium", "low":
	default:
		conf = "low"
	}
	return &models.PatternAnalysis{
		Subject:            subject,
		GeneratedAt:        time.Now().UTC().Format(time.RFC3339),
		BuildsAnalyzed:     builds,
		Systemic:           p.Systemic,
		Confidence:         conf,
		SharedRootCause:    strings.TrimSpace(p.SharedRootCause),
		SharedBuilds:       p.SharedBuilds,
		SuggestedFix:       strings.TrimSpace(p.SuggestedFix),
		RemediationTargets: append([]models.RemediationTarget(nil), p.RemediationTargets...),
		Summary:            strings.TrimSpace(p.Summary),
		RelevantFiles:      relevantFiles,
		FileLinks:          fileLinks,
		SourceRef:          sourceRef,
	}
}

// buildPatternUserPrompt renders the per-build analyses into the user message.
func buildPatternUserPrompt(subject string, failures []PatternFailure) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Job: %s\n", subject)
	fmt.Fprintf(&b, "It failed in %d recent builds. The per-build analyses follow (the failing test/spec may differ between builds).\n\n", len(failures))
	for i, f := range failures {
		fmt.Fprintf(&b, "--- Build %d (id %s) ---\n", i+1, f.BuildID)
		if f.FailingTest != "" {
			fmt.Fprintf(&b, "failing_test: %s\n", f.FailingTest)
		}
		if f.IsTransient {
			b.WriteString("classified_transient: yes\n")
		}
		if f.Severity != "" {
			fmt.Fprintf(&b, "severity: %s\n", f.Severity)
		}
		if f.RootCause != "" {
			fmt.Fprintf(&b, "root_cause: %s\n", clampPattern(f.RootCause, 1500))
		}
		if f.SuggestedFix != "" {
			fmt.Fprintf(&b, "suggested_fix: %s\n", clampPattern(f.SuggestedFix, 600))
		}
		if len(f.RelevantFiles) > 0 {
			fmt.Fprintf(&b, "relevant_files: %s\n", clampPattern(strings.Join(f.RelevantFiles, ", "), 400))
		}
		if f.FailureMessage != "" {
			fmt.Fprintf(&b, "failure_message: %s\n", clampPattern(f.FailureMessage, 600))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// patternCacheKey keys a verdict by the project module, job, prompt version,
// the grounding namespace (mode plus, when grounded, the source repo), and the
// rendered model input, so the verdict is reused only while the exact evidence
// the model saw is unchanged.
func patternCacheKey(module, generation, jobID, subject, userPrompt, groundKey, modelFingerprint string) string {
	h := sha256.New()
	fmt.Fprintf(h, "v%d:r%d\x00%s\x00%s\x00%s\x00%s\x00%s", patternPromptVersion, patternRepairVersion, groundKey, modelFingerprint, jobID, subject, userPrompt)
	digest := hex.EncodeToString(h.Sum(nil)[:12])
	if generation == "" {
		return fmt.Sprintf("pattern:%s:%s", module, digest)
	}
	return fmt.Sprintf("pattern:%s:g:%s:%s", module, generation, digest)
}

// clampPattern trims a field to max bytes so one verbose analysis can't blow
// the pattern prompt budget.
func clampPattern(s string, max int) string {
	return textutil.Truncate(strings.TrimSpace(s), max)
}

// patternPathRe matches repo-relative-looking file paths embedded in prose,
// with or without a directory prefix, ending in a source or config extension.
// Bounded to real file references so it does not flag incidental words. Runtime
// artifact extensions are excluded because suggested_fix names files to change.
var patternPathRe = regexp.MustCompile(`(?:[A-Za-z0-9_.\-]+/)*[A-Za-z0-9_.\-]+\.(?:go|ya?ml|sh|json|tpl|star|bzl|toml|cfg|conf|mod|sum|md|py|js|jsx|ts|tsx|java|rs|c|cc|cpp|h|hpp|proto|sql)\b`)

// guardPatternPaths annotates any file path in the verdict's suggested fix that
// does not exist in the source repo, so a fabricated citation is marked
// "(unverified path)" rather than asserted as fact. It is a no-op when no repo
// is available to verify against. Only suggested_fix is guarded: shared_root_cause
// and summary describe evidence and legitimately cite GCS artifact paths that are
// not in the source tree.
func (s *Service) guardPatternPaths(ctx context.Context, p *patternResponse) {
	exists := s.patternPathVerifier(ctx)
	if exists == nil {
		return
	}
	p.SuggestedFix = annotateUnverifiedPaths(p.SuggestedFix, exists)
}

// patternPathVerifier returns a func reporting whether a cited path exists, or
// nil when nothing can verify. The memoized repo tree verifies both full paths
// and bare basenames; if the tree is unavailable it falls back to a raw-CDN
// existence check against the configured analysis source so the guard stays active rather
// than silently disabling. The CDN fallback verifies only explicit repo-relative
// paths, leaving bare names unflagged since it cannot locate them to a subdir.
func (s *Service) patternPathVerifier(ctx context.Context) func(string) bool {
	if s.patternRepo != nil {
		if tree, err := s.patternRepoTree(ctx); err == nil && len(tree) > 0 {
			full := make(map[string]bool, len(tree))
			base := make(map[string]bool, len(tree))
			for _, t := range tree {
				full[t] = true
				base[path.Base(t)] = true
			}
			return func(p string) bool {
				if strings.Contains(p, "/") {
					return full[p]
				}
				return base[p]
			}
		}
	}
	if s.sourceRepoOwner != "" && s.sourceRepoName != "" {
		client := &http.Client{Timeout: 10 * time.Second}
		return func(p string) bool {
			if !strings.Contains(p, "/") {
				return true
			}
			return s.verifyGitHubFile(ctx, client, s.sourceRepoOwner, s.sourceRepoName, p)
		}
	}
	return nil
}

// annotateUnverifiedPaths marks each path-like token that fails exists with a
// trailing "(unverified path)" note, once per distinct token.
func annotateUnverifiedPaths(text string, exists func(string) bool) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	seen := map[string]bool{}
	for _, m := range patternPathRe.FindAllString(text, -1) {
		clean := strings.TrimPrefix(m, "./")
		if seen[m] || exists(clean) {
			seen[m] = true
			continue
		}
		seen[m] = true
		text = strings.ReplaceAll(text, m, m+" (unverified path)")
	}
	return text
}

// patternRepoTree returns the source repo's blob paths, memoized for the run so
// the recursive tree listing costs one API call across every job.
func (s *Service) patternRepoTree(ctx context.Context) ([]string, error) {
	s.patternTreeMu.Lock()
	defer s.patternTreeMu.Unlock()
	if s.patternTreeDone {
		return s.patternTree, s.patternTreeErr
	}
	s.patternTree, s.patternTreeErr = s.patternRepo.ListTree(ctx)
	s.patternTreeDone = true
	return s.patternTree, s.patternTreeErr
}
