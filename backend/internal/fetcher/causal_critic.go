package fetcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/causalcritic"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
)

func normalizeCausalCriticOptions(cfg *CausalCriticOptions) {
	if cfg == nil {
		return
	}
	cfg.LedgerPath = strings.TrimSpace(cfg.LedgerPath)
	if cfg.LedgerPath != "" {
		cfg.LedgerPath = filepath.Clean(cfg.LedgerPath)
	}
	cfg.ModelGateway.Endpoint = strings.TrimSpace(cfg.ModelGateway.Endpoint)
	cfg.ModelGateway.Model = strings.TrimSpace(cfg.ModelGateway.Model)
	cfg.ModelGateway.ProtocolVersion = strings.TrimSpace(cfg.ModelGateway.ProtocolVersion)
}

func validateCausalCriticOptions(opts Options) error {
	cfg := opts.CausalCritic
	if !cfg.Enabled {
		return nil
	}
	switch {
	case !opts.EnableAI:
		return fmt.Errorf("causal critic shadow requires -ai")
	case opts.AnalysisRuntime.Type != AnalysisRuntimeInProcess:
		return fmt.Errorf("causal critic shadow requires authoritative inprocess analysis")
	case opts.ShadowAnalysis.Enabled:
		return fmt.Errorf("causal critic shadow cannot run with the Orka agent analysis shadow")
	case cfg.LedgerPath == "" || !filepath.IsAbs(cfg.LedgerPath):
		return fmt.Errorf("causal critic shadow private ledger path must be absolute")
	case cfg.MaxPerRun < 1 || cfg.MaxPerRun > 10:
		return fmt.Errorf("causal critic shadow max per run must be between 1 and 10")
	case cfg.Timeout <= 0 || cfg.Timeout > 30*time.Minute:
		return fmt.Errorf("causal critic shadow timeout must be greater than zero and at most 30m")
	case cfg.OutputLimitBytes < 4<<10 || cfg.OutputLimitBytes > 1<<20:
		return fmt.Errorf("causal critic shadow output limit must be between 4096 and 1048576")
	}
	if err := agentanalysis.ValidatePrivateLedgerPath(opts.OutDir, cfg.LedgerPath); err != nil {
		return fmt.Errorf("causal critic shadow private ledger: %w", err)
	}
	if err := causalcritic.ValidateGatewayConfig(cfg.ModelGateway); err != nil {
		return err
	}
	if err := runtime.ValidateModelGatewayTrust(cfg.ModelGateway.Endpoint, false); err != nil {
		return fmt.Errorf("causal critic shadow gateway: %w", err)
	}
	return nil
}

func (p *pipeline) runCausalCritic(ctx context.Context, result *refreshResult) {
	if p == nil || !p.opts.CausalCritic.Enabled || result == nil {
		return
	}
	reviewer, err := p.ensureCausalCriticReviewer()
	if err != nil {
		log.Printf("🧪 causal critic shadow: runtime unavailable: %v", err)
		return
	}
	if cleaner, ok := reviewer.(causalcritic.PendingCleaner); ok {
		if err := causalcritic.RecoverPendingCleanup(ctx, cleaner, p.opts.OutDir, p.opts.CausalCritic.LedgerPath); err != nil {
			log.Printf("⚠ causal critic shadow cleanup retry: %v", err)
		}
	}
	candidates := p.selectShadowCandidates(result.details, result.flakiness)
	if len(candidates) == 0 {
		log.Printf("🧪 causal critic shadow: no eligible authoritative failures")
		return
	}
	attempted := 0
	for _, candidate := range candidates {
		if attempted >= p.opts.CausalCritic.MaxPerRun {
			break
		}
		if p.runCausalCriticCandidate(ctx, candidate) {
			attempted++
		}
	}
}

func (p *pipeline) runCausalCriticCandidate(ctx context.Context, candidate shadowCandidate) bool {
	cfg := p.opts.CausalCritic
	reviewer, err := p.ensureCausalCriticReviewer()
	if err != nil {
		log.Printf("🧪 causal critic shadow: runtime unavailable: %v", err)
		return false
	}
	now := time.Now
	if p.criticNow != nil {
		now = p.criticNow
	}
	preflightIdentity, err := causalcritic.PreflightIdentity(causalcritic.PreflightIdentityInput{
		RequestHash: candidate.requestHash, AuthoritativeHash: candidate.authoritativeHash,
		SourceRevision: candidate.source.Revision, SkillHash: p.aiProject.SkillSet.Hash(), RuntimeIdentity: reviewer.RuntimeIdentity(),
	})
	if err != nil {
		log.Printf("🧪 causal critic shadow: preflight identity invalid job=%s build=%s test=%s: %v", candidate.subject.JobID, candidate.subject.BuildID, candidate.subject.TestName, err)
		return false
	}
	preflight, claimed, err := causalcritic.ClaimPreflightAttempt(p.opts.OutDir, cfg.LedgerPath, preflightIdentity, now())
	if err != nil {
		log.Printf("⚠ causal critic shadow preflight claim failed job=%s build=%s test=%s: %v", candidate.subject.JobID, candidate.subject.BuildID, candidate.subject.TestName, err)
		return false
	}
	if !claimed {
		log.Printf("🧪 causal critic shadow: preflight already recorded status=%s job=%s build=%s test=%s", preflight.Status, candidate.subject.JobID, candidate.subject.BuildID, candidate.subject.TestName)
		return false
	}
	completePreflight := func(status causalcritic.PreflightStatus, failureCode, trialAttemptHash string) {
		if err := causalcritic.CompletePreflightAttempt(p.opts.OutDir, cfg.LedgerPath, preflightIdentity, status, failureCode, trialAttemptHash, now()); err != nil {
			log.Printf("⚠ causal critic shadow preflight completion failed job=%s build=%s test=%s: %v", candidate.subject.JobID, candidate.subject.BuildID, candidate.subject.TestName, err)
		}
	}
	evidenceTimeout := min(cfg.Timeout, 2*time.Minute)
	evidenceCtx, cancel := context.WithTimeout(ctx, evidenceTimeout)
	defer cancel()
	freeze := agentanalysis.FreezeEvidence
	if p.criticFreeze != nil {
		freeze = p.criticFreeze
	}
	browser := artifacts.NewUncachedBackendBrowser(p.backend, p.cfg.Storage.Bucket, candidate.request.BuildPrefix, candidate.request.Build.JobName+"/"+candidate.request.Build.BuildID)
	bundle, err := freeze(evidenceCtx, browser, candidate.request, candidate.source, p.aiProject.SkillSet)
	failureCode := "evidence_freeze"
	if err == nil {
		bundle, err = causalcritic.EnsureCitedEvidence(evidenceCtx, browser, bundle, candidate.authoritative.EvidenceCitations)
		failureCode = "evidence_citation"
	}
	if err != nil {
		completePreflight(causalcritic.PreflightEvidenceFailed, failureCode, "")
		log.Printf("🧪 causal critic shadow: evidence failed job=%s build=%s test=%s", candidate.subject.JobID, candidate.subject.BuildID, candidate.subject.TestName)
		return false
	}
	input, err := causalcritic.NewInput(bundle, candidate.authoritative)
	if err != nil {
		failureCode := "input_invalid"
		if code := causalcritic.ValidationCodeOf(err); code != "" {
			failureCode = "validation_" + string(code)
		}
		completePreflight(causalcritic.PreflightInputInvalid, failureCode, "")
		log.Printf("🧪 causal critic shadow: paired input invalid job=%s build=%s test=%s: %v", candidate.subject.JobID, candidate.subject.BuildID, candidate.subject.TestName, err)
		return false
	}
	caseID, stableID := causalCriticCaseIdentity(candidate.subject)
	metadata := causalcritic.TrialMetadata{
		CaseID: caseID, StableID: stableID, Repetition: 1, Arm: "agent-sandbox-independent-critic", AuthoritativeArm: "published",
		AuthoritativeElapsedMs: candidate.authoritative.ElapsedMs, AuthoritativeInputTokens: candidate.authoritative.InputTokens,
		AuthoritativeOutputTokens: candidate.authoritative.OutputTokens, AuthoritativeModelRequests: candidate.authoritative.ModelRequests,
		SameModelJudgeObjected: candidate.authoritative.JudgeObjected, SameModelJudgeRevised: candidate.authoritative.JudgeRevised,
	}
	executionID := "critic-" + input.PairHash[:16]
	record, runErr := causalcritic.RunTrial(ctx, reviewer, causalcritic.TrialSpec{
		PublicDir: p.opts.OutDir, LedgerPath: cfg.LedgerPath, Metadata: metadata, Input: input,
		ExecutionID: executionID, RuntimeIdentity: reviewer.RuntimeIdentity(), Now: now,
	})
	preflightFailureCode := record.FailureCode
	if preflightFailureCode == "" {
		preflightFailureCode = record.ErrorCode
	}
	if record.AttemptHash != "" && record.Status != causalcritic.TrialPending {
		completePreflight(causalcritic.PreflightSubmitted, preflightFailureCode, record.AttemptHash)
	}
	if errors.Is(runErr, causalcritic.ErrTrialAlreadyAttempted) {
		return false
	}
	if runErr != nil {
		log.Printf("🧪 causal critic shadow: status=%s job=%s build=%s test=%s error=%s", record.Status, candidate.subject.JobID, candidate.subject.BuildID, candidate.subject.TestName, record.ErrorCode)
		return true
	}
	log.Printf("🧪 causal critic shadow: status=%s verdict=%s job=%s build=%s test=%s", record.Status, record.Review.Verdict, candidate.subject.JobID, candidate.subject.BuildID, candidate.subject.TestName)
	return true
}

func causalCriticCaseIdentity(subject agentanalysis.Subject) (string, string) {
	caseID := strings.Join([]string{subject.JobID, subject.BuildID, subject.TestName}, "/")
	stable := sha256.Sum256([]byte(caseID))
	stableID := hex.EncodeToString(stable[:10])
	if len(caseID) > 160 || strings.ContainsAny(caseID, "\r\n\x00") {
		caseID = "critic-" + stableID
	}
	return caseID, stableID
}

func (p *pipeline) ensureCausalCriticReviewer() (causalcritic.Reviewer, error) {
	if p.criticReviewer != nil {
		return p.criticReviewer, nil
	}
	cfg := p.opts.CausalCritic
	runner, err := fixruntime.NewAgentSandboxRunnerFromEnv("AGENT_SANDBOX_CRITIC_", cfg.ModelGateway, false, cfg.Timeout, cfg.OutputLimitBytes)
	if err != nil {
		return nil, err
	}
	p.criticReviewer = &causalcritic.Runtime{Sandbox: runner, Gateway: cfg.ModelGateway, Timeout: cfg.Timeout, OutputLimitBytes: cfg.OutputLimitBytes}
	return p.criticReviewer, nil
}
