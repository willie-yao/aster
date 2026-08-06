package fetcher

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/prowbuild"
	agentruntime "github.com/willie-yao/prow-ai-dashboard/backend/internal/runtime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/sourceinvestigation"
)

type shadowAnalysisRunner interface {
	Generate(context.Context, agentanalysis.Spec) (agentanalysis.Result, error)
}

type shadowEvidenceFreezer func(context.Context, artifacts.Browser, ai.FailureAnalysisRequest, sourceinvestigation.Repository, *skills.Set) (agentanalysis.EvidenceBundle, error)
type shadowLedgerAppender func(string, agentanalysis.ShadowRecord) error
type shadowLedgerContains func(string, string) (bool, error)

type shadowCandidate struct {
	sortKey           string
	request           ai.FailureAnalysisRequest
	source            sourceinvestigation.Repository
	subject           agentanalysis.Subject
	authoritative     agentanalysis.AuthoritativeSnapshot
	requestHash       string
	authoritativeHash string
}

func normalizeShadowAnalysisOptions(cfg *ShadowAnalysisOptions) {
	if cfg == nil {
		return
	}
	cfg.Namespace = strings.TrimSpace(cfg.Namespace)
	cfg.ResultAPI = strings.TrimSpace(cfg.ResultAPI)
	cfg.AgentRef = strings.TrimSpace(cfg.AgentRef)
	cfg.AgentVersion = strings.TrimSpace(cfg.AgentVersion)
	cfg.GitSecret = strings.TrimSpace(cfg.GitSecret)
	cfg.KubeContext = strings.TrimSpace(cfg.KubeContext)
	cfg.LedgerPath = strings.TrimSpace(cfg.LedgerPath)
	if cfg.LedgerPath != "" {
		cfg.LedgerPath = filepath.Clean(cfg.LedgerPath)
	}
}

func validateShadowAnalysisOptions(opts Options) error {
	cfg := opts.ShadowAnalysis
	if !cfg.Enabled {
		return nil
	}
	switch {
	case !opts.EnableAI:
		return fmt.Errorf("agent analysis shadow requires -ai")
	case opts.AnalysisRuntime.Type != AnalysisRuntimeInProcess:
		return fmt.Errorf("agent analysis shadow requires authoritative inprocess analysis")
	case strings.TrimSpace(cfg.Namespace) == "":
		return fmt.Errorf("agent analysis shadow namespace is required")
	case strings.TrimSpace(cfg.ResultAPI) == "":
		return fmt.Errorf("agent analysis shadow result API is required")
	case strings.TrimSpace(cfg.AgentRef) == "":
		return fmt.Errorf("agent analysis shadow Agent reference is required")
	case strings.TrimSpace(cfg.AgentVersion) == "":
		return fmt.Errorf("agent analysis shadow Agent version is required")
	case strings.TrimSpace(cfg.LedgerPath) == "":
		return fmt.Errorf("agent analysis shadow private ledger path is required")
	case !filepath.IsAbs(cfg.LedgerPath):
		return fmt.Errorf("agent analysis shadow private ledger path must be absolute")
	case !shadowLedgerOutsideOutput(opts.OutDir, cfg.LedgerPath):
		return fmt.Errorf("agent analysis shadow ledger must be outside the public output directory")
	case cfg.MaxPerRun < 1 || cfg.MaxPerRun > 10:
		return fmt.Errorf("agent analysis shadow max per run must be between 1 and 10")
	case cfg.MaxTurns < 1 || cfg.MaxTurns > 1000:
		return fmt.Errorf("agent analysis shadow max turns must be between 1 and 1000")
	case cfg.Timeout <= 0 || cfg.Timeout > 30*time.Minute:
		return fmt.Errorf("agent analysis shadow timeout must be greater than zero and at most 30m")
	case cfg.Retries < 0 || cfg.Retries > 2:
		return fmt.Errorf("agent analysis shadow retries must be between 0 and 2")
	}
	resultAPI, err := url.ParseRequestURI(cfg.ResultAPI)
	if err != nil || resultAPI.Host == "" || resultAPI.User != nil || resultAPI.RawQuery != "" || resultAPI.Fragment != "" || resultAPI.Scheme != "http" && resultAPI.Scheme != "https" {
		return fmt.Errorf("agent analysis shadow result API must be an absolute HTTP or HTTPS URL without credentials or query parameters")
	}
	return nil
}

func (p *pipeline) runShadowAnalysis(ctx context.Context, result *refreshResult) {
	if p == nil || !p.opts.ShadowAnalysis.Enabled || result == nil {
		return
	}
	candidates := p.selectShadowCandidates(result.details, result.flakiness)
	if len(candidates) == 0 {
		log.Printf("🧪 agent analysis shadow: no eligible pinned failures")
		return
	}
	contains := p.shadowContains
	if contains == nil {
		attempts, err := agentanalysis.LedgerAttemptHashes(p.opts.ShadowAnalysis.LedgerPath)
		if err != nil {
			log.Printf("⚠ agent analysis shadow ledger read failed: %v", err)
			return
		}
		contains = func(_ string, attemptHash string) (bool, error) { return attempts[attemptHash], nil }
	}
	attempted := 0
	for _, candidate := range candidates {
		if attempted >= p.opts.ShadowAnalysis.MaxPerRun {
			break
		}
		if p.runShadowCandidate(ctx, candidate, contains) {
			attempted++
		}
	}
}

func (p *pipeline) selectShadowCandidates(details []models.JobDetail, report models.FlakinessReport) []shadowCandidate {
	if p == nil || p.aiProject == nil || p.aiProject.Config == nil || p.aiProject.Config.AI == nil {
		return nil
	}
	consecutive := map[string]int{}
	for _, failure := range report.PersistentFailures {
		consecutive[failure.JobID+"::"+failure.TestName] = failure.ConsecutiveFailures
	}
	owner, name := p.aiProject.AnalysisSource.Owner, p.aiProject.AnalysisSource.Name
	var candidates []shadowCandidate
	for di := range details {
		detail := &details[di]
		jobLocation := prowbuild.JobLocation{JobType: detail.JobType, Repo: detail.Repo}
		for ri := range detail.Runs {
			run := &detail.Runs[ri]
			source, ok := ai.ResolveBuildSource(run.BuildInfo, owner, name)
			if !ok || len(source.Revision) != 40 {
				continue
			}
			location := prowbuild.BuildLocation{
				JobLocation: jobLocation, JobName: detail.Name,
				BuildID: run.BuildID, PullNumber: run.PullNumber,
			}
			for ti := range run.TestCases {
				testCase := &run.TestCases[ti]
				if testCase.Status != "failed" || testCase.AISummary == nil || testCase.AIAnalysis == nil || testCase.AIAnalysis.Mode != ai.AgenticMode || !testCase.AIAnalysis.CritiquePassed {
					continue
				}
				authoritative, authoritativeHash, err := agentanalysis.NewAuthoritativeSnapshot(testCase.AISummary, testCase.AIAnalysis)
				if err != nil {
					continue
				}
				subject := agentanalysis.Subject{
					JobID: detail.JobID, BuildID: run.BuildID,
					TestName: testCase.Name, TestSource: testCase.Source,
					JUnitFile: testCase.JUnitFile, SuiteName: testCase.SuiteName, ClassName: testCase.ClassName,
				}
				build := run.BuildInfo
				build.RepoRefs = maps.Clone(run.RepoRefs)
				build.JUnitURLs = slices.Clone(run.JUnitURLs)
				sortKey := strings.Join([]string{detail.JobID, run.BuildID, testCase.Source, testCase.JUnitFile, testCase.SuiteName, testCase.ClassName, testCase.Name}, "\x00")
				request := ai.FailureAnalysisRequest{
					JobID: detail.JobID, BuildPrefix: location.BuildPath(), Build: build, TestCase: *testCase,
					ProwJob: &ai.ProwJobContext{
						Name: detail.Name, JobType: detail.JobType,
						ConfigFile: detail.ConfigFile, ConfigRevision: detail.ConfigRevision,
					},
					ConsecutiveFailures: consecutive[detail.JobID+"::"+testCase.Name],
					CacheGeneration:     p.cacheGenerationFingerprint(),
				}
				candidates = append(candidates, shadowCandidate{
					sortKey: sortKey, request: request,
					source:  sourceinvestigation.Repository{Owner: source.Owner, Name: source.Name, Revision: source.Revision},
					subject: subject, authoritative: authoritative, requestHash: agentanalysis.FailureRequestHash(request), authoritativeHash: authoritativeHash,
				})
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].sortKey < candidates[j].sortKey })
	deduped := candidates[:0]
	previous := ""
	for _, candidate := range candidates {
		if candidate.sortKey == previous {
			continue
		}
		previous = candidate.sortKey
		deduped = append(deduped, candidate)
	}
	return deduped
}

func (p *pipeline) runShadowCandidate(ctx context.Context, candidate shadowCandidate, contains shadowLedgerContains) bool {
	started := time.Now()
	cfg := p.opts.ShadowAnalysis
	now := time.Now
	if p.shadowNow != nil {
		now = p.shadowNow
	}
	createdAt := now().UTC()
	record := agentanalysis.ShadowRecord{
		CreatedAt: createdAt.Format(time.RFC3339Nano), Subject: candidate.subject,
		Source: candidate.source, Authoritative: candidate.authoritative,
		RequestHash: candidate.requestHash, AuthoritativeHash: candidate.authoritativeHash,
		Provenance: agentanalysis.Provenance{
			Runtime: "orka", AgentNamespace: cfg.Namespace, AgentRef: cfg.AgentRef, AgentVersion: cfg.AgentVersion, GitSecret: cfg.GitSecret,
			ContractVersion: agentanalysis.ContractVersion, SourceSHA: candidate.source.Revision,
			Timeout: cfg.Timeout.String(), MaxTurns: cfg.MaxTurns, Retries: cfg.Retries,
		},
	}
	skillSetHash := ""
	if p.aiProject.SkillSet != nil {
		skillSetHash = p.aiProject.SkillSet.Hash()
	}
	record.AttemptHash = agentanalysis.AttemptIdentity(candidate.subject, candidate.requestHash, candidate.authoritativeHash, skillSetHash, candidate.source, cfg.Namespace, cfg.AgentRef, cfg.AgentVersion, cfg.GitSecret, cfg.Timeout, cfg.MaxTurns, cfg.Retries)
	record.ID = agentanalysis.NewRecordID(candidate.subject, createdAt, record.AttemptHash)
	alreadyAttempted, ledgerErr := contains(cfg.LedgerPath, record.AttemptHash)
	if ledgerErr != nil {
		log.Printf("⚠ agent analysis shadow ledger read failed: %v", ledgerErr)
		return false
	}
	if alreadyAttempted {
		log.Printf("🧪 agent analysis shadow: exact comparison already attempted job=%s build=%s test=%s", record.Subject.JobID, record.Subject.BuildID, record.Subject.TestName)
		return false
	}
	shadowCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	freeze := agentanalysis.FreezeEvidence
	if p.shadowFreeze != nil {
		freeze = p.shadowFreeze
	}
	browser := artifacts.NewUncachedBackendBrowser(p.backend, p.cfg.Storage.Bucket, candidate.request.BuildPrefix, candidate.request.Build.JobName+"/"+candidate.request.Build.BuildID)
	bundle, err := freeze(shadowCtx, browser, candidate.request, candidate.source, p.aiProject.SkillSet)
	if err != nil {
		record.Status, record.ErrorCode = classifyShadowFailure(err, true)
		record.TotalDurationMs = time.Since(started).Milliseconds()
		p.appendShadowRecord(record)
		log.Printf("🧪 agent analysis shadow: status=%s job=%s build=%s test=%s", record.Status, record.Subject.JobID, record.Subject.BuildID, record.Subject.TestName)
		return true
	}
	record.Scan = &bundle.Scan
	record.Evidence, record.PlanIDs = agentanalysis.EvidenceManifest(bundle)
	record.ComparisonHash = agentanalysis.ComparisonIdentity(record.AttemptHash, bundle)
	runner, setupErr := p.ensureShadowRunner()
	if setupErr != nil || runner == nil {
		record.Status, record.ErrorCode = agentanalysis.ShadowStatusSetupFailed, "runtime_setup"
		record.TotalDurationMs = time.Since(started).Milliseconds()
		p.appendShadowRecord(record)
		log.Printf("🧪 agent analysis shadow: status=%s job=%s build=%s test=%s", record.Status, record.Subject.JobID, record.Subject.BuildID, record.Subject.TestName)
		return true
	}
	generated, runErr := runner.Generate(shadowCtx, agentanalysis.Spec{
		Repo:   agentruntime.RepoRef{Owner: candidate.source.Owner, Name: candidate.source.Name, Ref: candidate.source.Revision},
		Bundle: bundle, SourceReader: orka.NewGitHubSourceReader("", os.Getenv("GITHUB_READ_TOKEN")), MaxTurns: cfg.MaxTurns, Timeout: cfg.Timeout,
	})
	record.Provenance = agentanalysis.ProvenanceFromResult(generated)
	record.Provenance.GitSecret = cfg.GitSecret
	record.CleanupPending = generated.CleanupPending
	record.CleanupWork = generated.CleanupWork
	if generated.Analysis.Summary != "" {
		analysis := generated.Analysis
		record.Shadow = &analysis
	}
	switch {
	case runErr == nil:
		record.Status = agentanalysis.ShadowStatusSucceeded
	case errors.Is(runErr, agentruntime.ErrCleanupPending) && record.Shadow != nil:
		record.Status, record.ErrorCode = agentanalysis.ShadowStatusCleanupPending, "cleanup_pending"
	default:
		record.Status, record.ErrorCode = classifyShadowFailure(runErr, false)
	}
	record.TotalDurationMs = time.Since(started).Milliseconds()
	p.appendShadowRecord(record)
	log.Printf("🧪 agent analysis shadow: status=%s job=%s build=%s test=%s", record.Status, record.Subject.JobID, record.Subject.BuildID, record.Subject.TestName)
	return true
}

func (p *pipeline) ensureShadowRunner() (shadowAnalysisRunner, error) {
	if p.shadowRunner != nil {
		return p.shadowRunner, nil
	}
	cfg := p.opts.ShadowAnalysis
	agent, err := orka.NewAgentRuntimeFromEnv(orka.FromEnvConfig{
		Namespace: cfg.Namespace, AgentRef: cfg.AgentRef, GitSecret: cfg.GitSecret,
		API: cfg.ResultAPI, Version: cfg.AgentVersion, MaxRetries: cfg.Retries,
		Purpose: orka.AgentPurposeFailureAnalysis, KubeContext: cfg.KubeContext,
	})
	if err != nil {
		return nil, err
	}
	p.shadowRunner = &agentanalysis.Runtime{
		Agent: agent, Name: "orka", AgentNamespace: cfg.Namespace, AgentRef: cfg.AgentRef,
		AgentVersion: cfg.AgentVersion, Retries: cfg.Retries,
	}
	return p.shadowRunner, nil
}

func (p *pipeline) appendShadowRecord(record agentanalysis.ShadowRecord) {
	appendLedger := agentanalysis.AppendLedger
	if p.shadowAppend != nil {
		appendLedger = p.shadowAppend
	}
	if err := appendLedger(p.opts.ShadowAnalysis.LedgerPath, record); err != nil {
		log.Printf("⚠ agent analysis shadow ledger write failed: %v", err)
	}
}

func shadowLedgerOutsideOutput(outDir, ledgerPath string) bool {
	outAbs, err := filepath.Abs(filepath.Clean(outDir))
	if err != nil {
		return false
	}
	ledgerAbs, err := filepath.Abs(filepath.Clean(ledgerPath))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(outAbs, ledgerAbs)
	if err != nil {
		return false
	}
	return rel != "." && rel != "" && rel != ".." && strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func classifyShadowFailure(err error, evidence bool) (agentanalysis.ShadowStatus, string) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return agentanalysis.ShadowStatusCancelled, "cancelled"
	case evidence || errors.Is(err, agentanalysis.ErrEvidenceUnavailable), errors.Is(err, agentanalysis.ErrInvalidBundle):
		return agentanalysis.ShadowStatusEvidenceFailed, "evidence"
	case errors.Is(err, agentanalysis.ErrInvalidResult):
		return agentanalysis.ShadowStatusInvalidResult, "invalid_result"
	default:
		return agentanalysis.ShadowStatusRuntimeFailed, "runtime"
	}
}
