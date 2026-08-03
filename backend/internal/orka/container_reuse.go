package orka

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	containerCompatibleResultCandidateLimit = 5
	containerCompatibleResultLookupTimeout  = 5 * time.Second
	// ContainerAnalysisSucceededTaskRetention bounds cross-image result reuse.
	ContainerAnalysisSucceededTaskRetention = 7 * 24 * time.Hour
	containerResultClockSkew                = 5 * time.Minute
)

var (
	containerAnalysisDigestPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	containerAnalysisWorkItemPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

type compatibleContainerResultCandidate struct {
	Namespace    string
	Name         string
	BundleDigest string
	CreatedAt    time.Time
	CompletedAt  time.Time
}

// ReuseExactResult returns a validated result for the current Task identity without creating resources.
func (a *ContainerAnalyzer) ReuseExactResult(ctx context.Context, request ai.FailureAnalysisRequest, policy ai.AgenticCachePolicy) (ai.FailureAnalysisResult, bool, error) {
	if a == nil || a.kube == nil || a.results == nil || a.state == nil {
		return ai.FailureAnalysisResult{}, false, fmt.Errorf("container analysis runtime is not configured")
	}
	taskRequest := analysisruntime.CanonicalFailureAnalysisRequest(request)
	prepared, err := prepareContainerAnalysisTask(a.taskSpec(taskRequest, a.state.CacheSeed(taskRequest), nil))
	if err != nil {
		return ai.FailureAnalysisResult{}, false, err
	}
	workItem, _ := containerAnalysisCorrelation(nil, taskRequest)
	task, err := a.kube.Get(ctx, TasksGVR, a.opts.Namespace, prepared.Name)
	if IsNotFound(err) {
		return ai.FailureAnalysisResult{}, false, nil
	}
	if err != nil {
		return ai.FailureAnalysisResult{}, false, fmt.Errorf("read exact container analysis Task: %w", err)
	}
	candidate, ok := compatibleContainerResultCandidateFromObject(
		task, a.opts.Namespace, workItem, containerStateKeyFingerprint(a.opts.StateKey), "", time.Now().UTC(),
	)
	if !ok || candidate.Name != prepared.Name || candidate.BundleDigest != prepared.BundleDigest {
		return ai.FailureAnalysisResult{}, false, nil
	}
	lookupCtx, cancelLookup := context.WithTimeout(ctx, containerCompatibleResultLookupTimeout)
	defer cancelLookup()
	return a.reuseContainerResultCandidate(ctx, lookupCtx, taskRequest, policy, candidate)
}

// ReuseCompatibleResult returns a validated succeeded result without creating a Task.
func (a *ContainerAnalyzer) ReuseCompatibleResult(ctx context.Context, request ai.FailureAnalysisRequest, policy ai.AgenticCachePolicy) (ai.FailureAnalysisResult, bool, error) {
	if a == nil || a.kube == nil || a.results == nil || a.state == nil {
		return ai.FailureAnalysisResult{}, false, fmt.Errorf("container analysis runtime is not configured")
	}
	taskRequest := analysisruntime.CanonicalFailureAnalysisRequest(request)
	prepared, err := prepareContainerAnalysisTask(a.taskSpec(taskRequest, nil, nil))
	if err != nil {
		return ai.FailureAnalysisResult{}, false, err
	}
	workItem, _ := containerAnalysisCorrelation(nil, taskRequest)
	candidates, err := compatibleContainerResultCandidates(
		ctx, a.kube, a.opts.Namespace, workItem,
		containerStateKeyFingerprint(a.opts.StateKey), prepared.Name, time.Now().UTC(),
	)
	if err != nil {
		if ctx.Err() != nil {
			return ai.FailureAnalysisResult{}, false, ctx.Err()
		}
		return ai.FailureAnalysisResult{}, false, nil
	}
	lookupCtx, cancelLookup := context.WithTimeout(ctx, containerCompatibleResultLookupTimeout)
	defer cancelLookup()
	for _, candidate := range candidates {
		result, reused, reuseErr := a.reuseContainerResultCandidate(ctx, lookupCtx, taskRequest, policy, candidate)
		if reuseErr != nil {
			return ai.FailureAnalysisResult{}, false, reuseErr
		}
		if reused {
			return result, true, nil
		}
	}
	return ai.FailureAnalysisResult{}, false, nil
}

func (a *ContainerAnalyzer) reuseContainerResultCandidate(
	ctx, lookupCtx context.Context,
	taskRequest ai.FailureAnalysisRequest,
	policy ai.AgenticCachePolicy,
	candidate compatibleContainerResultCandidate,
) (ai.FailureAnalysisResult, bool, error) {
	raw, ok, resultErr := a.results.Result(lookupCtx, candidate.Namespace, candidate.Name)
	if resultErr != nil {
		if IsResultAuthorizationError(resultErr) {
			return ai.FailureAnalysisResult{}, false, fmt.Errorf("read reusable container analysis result: %w", resultErr)
		}
		if ctx.Err() != nil {
			return ai.FailureAnalysisResult{}, false, ctx.Err()
		}
		return ai.FailureAnalysisResult{}, false, nil
	}
	if !ok {
		return ai.FailureAnalysisResult{}, false, nil
	}
	result, parseErr := ParseContainerAnalysisResult(raw)
	if parseErr != nil || ai.AgenticResultRejection(result, policy) != ai.CacheAccepted {
		return ai.FailureAnalysisResult{}, false, nil
	}
	identity := analysisruntime.NewContainerStateIdentity(candidate.Namespace, candidate.Name, taskRequest)
	delta, stateErr := analysisruntime.ParseEncryptedContainerAnalysisState(raw, a.opts.StateKey, identity)
	if stateErr != nil {
		if analysisruntime.IsContainerStateDecryptionError(stateErr) || analysisruntime.IsContainerStateIdentityError(stateErr) {
			return ai.FailureAnalysisResult{}, false, fmt.Errorf("validate reusable container analysis state: %w", stateErr)
		}
		return ai.FailureAnalysisResult{}, false, nil
	}
	cacheKey := analysisruntime.FailureCacheKey(taskRequest)
	entry, hasEntry := delta.CacheEntries[cacheKey]
	if hasEntry {
		cachedResult, reason := ai.AcceptAgenticCacheEntry(entry, cacheKey, policy)
		if reason != ai.CacheAccepted || !sameAgenticResult(cachedResult, result) {
			return ai.FailureAnalysisResult{}, false, nil
		}
	} else {
		var entryErr error
		entry, entryErr = ai.NewAgenticCacheEntry(cacheKey, result, candidate.CompletedAt)
		if entryErr != nil {
			return ai.FailureAnalysisResult{}, false, nil
		}
	}
	if err := a.state.StageCacheEntry(entry); err != nil {
		return ai.FailureAnalysisResult{}, false, fmt.Errorf("stage reusable container analysis cache: %w", err)
	}
	return result, true, nil
}

func compatibleContainerResultCandidates(
	ctx context.Context,
	client ContainerAnalysisResourceClient,
	namespace, workItem, stateKeyFingerprint, currentTaskName string,
	now time.Time,
) ([]compatibleContainerResultCandidate, error) {
	if client == nil || strings.TrimSpace(namespace) == "" {
		return nil, fmt.Errorf("container analysis candidate lookup is not configured")
	}
	if !containerAnalysisWorkItemPattern.MatchString(workItem) || !containerAnalysisDigestPattern.MatchString(stateKeyFingerprint) {
		return nil, fmt.Errorf("container analysis candidate identity is invalid")
	}
	selector := strings.Join([]string{
		containerAnalysisTaskSelector,
		containerWorkItemLabel + "=" + workItem,
		"app.kubernetes.io/managed-by=prow-ai-dashboard",
	}, ",")
	items, err := client.ListByLabel(ctx, TasksGVR, namespace, selector)
	if err != nil {
		return nil, fmt.Errorf("list compatible container analysis Tasks: %w", err)
	}
	candidates := make([]compatibleContainerResultCandidate, 0, min(len(items), containerCompatibleResultCandidateLimit))
	for i := range items {
		candidate, ok := compatibleContainerResultCandidateFromObject(
			&items[i], namespace, workItem, stateKeyFingerprint, currentTaskName, now,
		)
		if ok {
			candidates = append(candidates, candidate)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].CompletedAt.Equal(candidates[j].CompletedAt) {
			return candidates[i].CompletedAt.After(candidates[j].CompletedAt)
		}
		if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
		}
		return candidates[i].Name < candidates[j].Name
	})
	if len(candidates) > containerCompatibleResultCandidateLimit {
		candidates = candidates[:containerCompatibleResultCandidateLimit]
	}
	return candidates, nil
}

func compatibleContainerResultCandidateFromObject(
	task *unstructured.Unstructured,
	namespace, workItem, stateKeyFingerprint, currentTaskName string,
	now time.Time,
) (compatibleContainerResultCandidate, bool) {
	if task == nil || task.GetAPIVersion() != "core.orka.ai/v1alpha1" || task.GetKind() != "Task" || task.GetNamespace() != namespace || task.GetName() == "" || task.GetName() == currentTaskName {
		return compatibleContainerResultCandidate{}, false
	}
	candidateWorkItem, candidateBundle, candidateFingerprint, ok := reusableContainerAnalysisTaskMetadata(task)
	if !ok || candidateWorkItem != workItem || candidateFingerprint != stateKeyFingerprint {
		return compatibleContainerResultCandidate{}, false
	}
	state, err := taskStateFromObject(task)
	if err != nil || state.Deleting || state.Phase != "Succeeded" || !state.ResultAvailable || state.CompletionTime.IsZero() {
		return compatibleContainerResultCandidate{}, false
	}
	createdAt := task.GetCreationTimestamp().Time
	if createdAt.IsZero() || state.CompletionTime.After(now.Add(containerResultClockSkew)) || now.Sub(state.CompletionTime) > ContainerAnalysisSucceededTaskRetention {
		return compatibleContainerResultCandidate{}, false
	}
	return compatibleContainerResultCandidate{
		Namespace: namespace, Name: task.GetName(), BundleDigest: candidateBundle, CreatedAt: createdAt, CompletedAt: state.CompletionTime,
	}, true
}

func reusableContainerAnalysisTaskMetadata(task *unstructured.Unstructured) (workItem, bundleDigest, stateKeyFingerprint string, ok bool) {
	if task == nil {
		return "", "", "", false
	}
	labels := task.GetLabels()
	workItem = labels[containerWorkItemLabel]
	if labels["app.kubernetes.io/managed-by"] != "prow-ai-dashboard" || labels["prow-ai-dashboard/adapter"] != "container-analyzer" || !containerAnalysisWorkItemPattern.MatchString(workItem) {
		return "", "", "", false
	}
	annotations := task.GetAnnotations()
	bundleDigest = annotations["prow-ai-dashboard/bundle-digest"]
	stateKeyFingerprint = annotations["prow-ai-dashboard/state-key-fingerprint"]
	if annotations["prow-ai-dashboard/contract-version"] != ContainerAnalysisContractVersion || !containerAnalysisDigestPattern.MatchString(bundleDigest) || !containerAnalysisDigestPattern.MatchString(stateKeyFingerprint) {
		return "", "", "", false
	}
	return workItem, bundleDigest, stateKeyFingerprint, true
}

func sameAgenticResult(left, right ai.FailureAnalysisResult) bool {
	if left.Summary == nil || right.Summary == nil || left.Analysis == nil || right.Analysis == nil {
		return false
	}
	return left.Summary.GeneratedAt == right.Summary.GeneratedAt &&
		left.Summary.Summary == right.Summary.Summary &&
		left.Summary.IsTransient == right.Summary.IsTransient &&
		left.Analysis.GeneratedAt == right.Analysis.GeneratedAt &&
		left.Analysis.Mode == right.Analysis.Mode &&
		left.Analysis.RootCause == right.Analysis.RootCause &&
		left.Analysis.Severity == right.Analysis.Severity &&
		left.Analysis.SuggestedFix == right.Analysis.SuggestedFix &&
		slices.Equal(left.Analysis.RelevantFiles, right.Analysis.RelevantFiles) &&
		slices.Equal(left.Analysis.SearchSuggestions, right.Analysis.SearchSuggestions) &&
		slices.Equal(left.Analysis.EvidenceCitations, right.Analysis.EvidenceCitations) &&
		left.Analysis.ToolCalls == right.Analysis.ToolCalls &&
		left.Analysis.ContextBytes == right.Analysis.ContextBytes &&
		left.Analysis.GCSBytes == right.Analysis.GCSBytes &&
		left.Analysis.EvidencePlanCovered == right.Analysis.EvidencePlanCovered &&
		left.Analysis.BudgetExhausted == right.Analysis.BudgetExhausted &&
		left.Analysis.SameFailureReuse == right.Analysis.SameFailureReuse &&
		left.Analysis.CritiquePassed == right.Analysis.CritiquePassed &&
		left.Analysis.CritiqueVersion == right.Analysis.CritiqueVersion &&
		left.Analysis.SkillSetHash == right.Analysis.SkillSetHash &&
		left.Analysis.ModelHash == right.Analysis.ModelHash &&
		left.Analysis.PromptHash == right.Analysis.PromptHash &&
		left.Analysis.CacheGeneration == right.Analysis.CacheGeneration
}
