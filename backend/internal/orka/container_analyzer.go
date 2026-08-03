package orka

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/analysisruntime"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fetchprogress"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

const (
	containerAnalyzerFieldManager = "prow-ai-dashboard-container-analysis"
	failedTaskStateTimeout        = 30 * time.Second
	containerTaskSchedulingGrace  = 2 * time.Minute
	containerTaskExecutionGrace   = 2 * time.Minute
	containerTaskRetryDelay       = 10 * time.Second
	containerResultReadTimeout    = 30 * time.Second
	containerResultPreflightTask  = "prow-ai-dashboard-result-access-preflight"
)

var immutableAnalyzerTagPattern = regexp.MustCompile(`^(sha-[0-9a-fA-F]{7,64}|v?[0-9]+[.][0-9]+[.][0-9]+(-[0-9A-Za-z.-]+)?)$`)

// ContainerAnalyzerOptions configures the experimental Orka container runtime.
type ContainerAnalyzerOptions struct {
	Namespace           string
	Image               string
	ProjectDir          string
	DataDir             string
	API                 string
	Endpoint            string
	Model               string
	CacheGeneration     string
	ModelSecretName     string
	ModelTokenKey       string
	GitHubSecretName    string
	GitHubTokenKey      string
	StateSecretName     string
	StateSecretKey      string
	StateKey            []byte
	ContextWindowTokens int
	AnalysisTimeout     time.Duration
	TaskTimeout         time.Duration
	PollInterval        time.Duration
	MaxRetries          int
	MaxConcurrentTasks  int
	NodeSelector        map[string]string
	Tolerations         []map[string]any
	Affinity            map[string]any
	Labels              map[string]string
	Progress            *fetchprogress.Tracker
	UsageRecorder       *aiusage.Recorder
	KubeContext         string
	OrkaAPI             string
	OrkaAPIToken        string
}

type containerAnalyzerKube interface {
	ContainerAnalysisResourceClient
	DeleteTaskIfIdentity(context.Context, string, string, string, string) (bool, error)
}

type containerAnalyzerResults interface {
	Result(context.Context, string, string) (string, bool, error)
}

// ContainerAnalyzer runs the dashboard-owned FailureAnalyzer in Orka container Tasks.
type ContainerAnalyzer struct {
	opts          ContainerAnalyzerOptions
	kube          containerAnalyzerKube
	results       containerAnalyzerResults
	state         *analysisruntime.ContainerStateStore
	sem           chan struct{}
	maintenanceMu sync.Mutex
}

// NewContainerAnalyzer builds the Kubernetes, Orka API, and encrypted-state clients.
func NewContainerAnalyzer(opts ContainerAnalyzerOptions) (*ContainerAnalyzer, error) {
	if err := validateContainerAnalyzerOptions(opts); err != nil {
		return nil, err
	}
	rc, err := RESTConfig(opts.KubeContext)
	if err != nil {
		return nil, fmt.Errorf("container analysis kube config: %w", err)
	}
	kube, err := NewKubeClient(rc)
	if err != nil {
		return nil, fmt.Errorf("container analysis kube client: %w", err)
	}
	kube.Manager = containerAnalyzerFieldManager
	state, err := analysisruntime.NewContainerStateStore(opts.DataDir, opts.UsageRecorder)
	if err != nil {
		return nil, fmt.Errorf("container analysis state: %w", err)
	}
	return newContainerAnalyzer(opts, kube, newResultClientFromEnv(strings.TrimSpace(opts.OrkaAPI), opts.OrkaAPIToken), state)
}

func newContainerAnalyzer(opts ContainerAnalyzerOptions, kube containerAnalyzerKube, results containerAnalyzerResults, state *analysisruntime.ContainerStateStore) (*ContainerAnalyzer, error) {
	if err := validateContainerAnalyzerOptions(opts); err != nil {
		return nil, err
	}
	if kube == nil || results == nil || state == nil {
		return nil, fmt.Errorf("container analysis clients and state store are required")
	}
	return &ContainerAnalyzer{
		opts: opts, kube: kube, results: results, state: state,
		sem: make(chan struct{}, opts.MaxConcurrentTasks),
	}, nil
}

func containerStateKeyFingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return fmt.Sprintf("%x", sum[:])
}

// ValidateContainerAnalyzerOptions validates a complete execution plan without creating clients.
func ValidateContainerAnalyzerOptions(opts ContainerAnalyzerOptions) error {
	return validateContainerAnalyzerOptions(opts)
}

func validateOrkaResultAPI(raw string) error {
	return validateResultAPI(raw, "container analysis Orka result API")
}

func immutableContainerAnalyzerImage(image string) bool {
	image = strings.TrimSpace(image)
	colon := strings.LastIndex(image, ":")
	slash := strings.LastIndex(image, "/")
	return colon > slash && immutableAnalyzerTagPattern.MatchString(image[colon+1:])
}

func validateContainerAnalyzerOptions(opts ContainerAnalyzerOptions) error {
	if err := project.ValidateAICacheGeneration(opts.CacheGeneration); err != nil {
		return fmt.Errorf("container analysis cache generation: %w", err)
	}
	if err := validateOrkaResultAPI(opts.OrkaAPI); err != nil {
		return err
	}
	if err := project.ValidateAIAPI(strings.ToLower(strings.TrimSpace(opts.API))); err != nil {
		return err
	}
	if _, err := validateContainerAnalysisEndpoint(opts.Endpoint); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(opts.Namespace) == "":
		return fmt.Errorf("container analysis namespace is required")
	case strings.TrimSpace(opts.Image) == "":
		return fmt.Errorf("container analysis image is required")
	case !immutableContainerAnalyzerImage(opts.Image):
		return fmt.Errorf("container analysis image tag must be an immutable sha-<hex> or full semantic version")
	case strings.TrimSpace(opts.ProjectDir) == "":
		return fmt.Errorf("container analysis project directory is required")
	case strings.TrimSpace(opts.DataDir) == "":
		return fmt.Errorf("container analysis data directory is required")
	case strings.TrimSpace(opts.API) == "":
		return fmt.Errorf("container analysis API mode is required")
	case strings.TrimSpace(opts.Endpoint) == "":
		return fmt.Errorf("container analysis endpoint is required")
	case strings.TrimSpace(opts.Model) == "":
		return fmt.Errorf("container analysis model is required")
	case strings.TrimSpace(opts.ModelSecretName) == "":
		return fmt.Errorf("container analysis model Secret is required")
	case strings.TrimSpace(opts.ModelTokenKey) == "":
		return fmt.Errorf("container analysis model token key is required")
	case (strings.TrimSpace(opts.GitHubSecretName) == "") != (strings.TrimSpace(opts.GitHubTokenKey) == ""):
		return fmt.Errorf("container analysis GitHub Secret name and token key must be configured together")
	case strings.TrimSpace(opts.StateSecretName) == "":
		return fmt.Errorf("container analysis state Secret is required")
	case strings.TrimSpace(opts.StateSecretKey) == "":
		return fmt.Errorf("container analysis state key name is required")
	case len(opts.StateKey) != 32:
		return fmt.Errorf("container analysis state key must be 32 bytes")
	case opts.AnalysisTimeout <= 0:
		return fmt.Errorf("container analysis inner timeout must be positive")
	case opts.TaskTimeout <= 0:
		return fmt.Errorf("container analysis task timeout must be positive")
	case opts.AnalysisTimeout > time.Duration(1<<63-1)-containerTaskExecutionGrace || opts.TaskTimeout < opts.AnalysisTimeout+containerTaskExecutionGrace:
		return fmt.Errorf("container analysis task timeout %s must be at least ai.timeout %s plus %s execution grace", opts.TaskTimeout, opts.AnalysisTimeout, containerTaskExecutionGrace)
	case opts.PollInterval <= 0:
		return fmt.Errorf("container analysis poll interval must be positive")
	case opts.PollInterval >= containerResultReadTimeout:
		return fmt.Errorf("container analysis poll interval must be less than %s", containerResultReadTimeout)
	case opts.MaxRetries < 0:
		return fmt.Errorf("container analysis retries must not be negative")
	case opts.MaxConcurrentTasks < 1:
		return fmt.Errorf("container analysis max concurrent Tasks must be positive")
	default:
		return validateContainerAnalysisPlacement(opts.NodeSelector, opts.Tolerations, opts.Affinity)
	}
}

// Maintain runs bounded bundle and terminal Task retention once per fetch pass.
func (a *ContainerAnalyzer) Maintain(ctx context.Context) error {
	if a == nil {
		return fmt.Errorf("container analysis runtime is not configured")
	}
	a.maintenanceMu.Lock()
	defer a.maintenanceMu.Unlock()
	now := time.Now().UTC()
	if _, err := PruneContainerAnalysisBundles(ctx, a.kube, a.opts.Namespace, now); err != nil {
		return fmt.Errorf("prune stale container analysis resources: %w", err)
	}
	if _, err := PruneContainerAnalysisTasks(ctx, a.kube, a.opts.Namespace, now); err != nil {
		return fmt.Errorf("prune stale container analysis resources: %w", err)
	}
	return nil
}

// Preflight verifies result API access before creating analyzer Tasks.
func (a *ContainerAnalyzer) Preflight(ctx context.Context) error {
	if a == nil {
		return fmt.Errorf("container analysis runtime is not configured")
	}
	preflightCtx, cancel := context.WithTimeout(ctx, containerResultReadTimeout)
	defer cancel()
	if _, _, err := a.results.Result(preflightCtx, a.opts.Namespace, containerResultPreflightTask); err != nil {
		return fmt.Errorf("container analysis result API preflight: %w", err)
	}
	return nil
}

// AnalyzeFailure implements ai.FailureAnalyzer using one content-addressed Task.
func (a *ContainerAnalyzer) AnalyzeFailure(ctx context.Context, _ *http.Client, request ai.FailureAnalysisRequest) (ai.FailureAnalysisResult, error) {
	if a == nil {
		err := fmt.Errorf("container analysis runtime is not configured")
		return ai.UnavailableFailureAnalysisResult(request.TestCase, err), err
	}
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	case <-ctx.Done():
		return ai.UnavailableFailureAnalysisResult(request.TestCase, ctx.Err()), ctx.Err()
	}

	taskRequest := analysisruntime.CanonicalFailureAnalysisRequest(request)
	cacheSeed := a.state.CacheSeed(taskRequest)
	workItem, correlationLabels := containerAnalysisCorrelation(a.opts.Progress, taskRequest)

	resources, err := BuildContainerAnalysisResources(a.taskSpec(taskRequest, cacheSeed, correlationLabels))
	if err != nil {
		return ai.UnavailableFailureAnalysisResult(request.TestCase, err), err
	}
	_, taskName, err := containerResourceRef(resources.Task)
	if err != nil {
		return ai.UnavailableFailureAnalysisResult(request.TestCase, err), err
	}
	if a.opts.Progress != nil {
		a.opts.Progress.RecordTaskPlanned(workItem, taskName, resources.CacheSeedIncluded, taskRequest.TestCase.Source == models.TestCaseSourceBuild)
	}
	reconcile, err := ReconcileContainerAnalysisResourcesWithResult(ctx, a.kube, resources)
	if err != nil {
		err = fmt.Errorf("reconcile container analysis Task %s: %w", taskName, err)
		return ai.UnavailableFailureAnalysisResult(request.TestCase, err), err
	}
	if a.opts.Progress != nil {
		a.opts.Progress.RecordTaskState(workItem, "Pending", 0, reconcile.Adopted)
	}

	taskCtx, cancel := context.WithTimeout(ctx, containerTaskWaitTimeout(a.opts.TaskTimeout, a.opts.MaxRetries))
	defer cancel()
	state, err := a.waitTerminal(taskCtx, taskName, workItem, reconcile.Adopted)
	if err != nil {
		if a.opts.Progress != nil {
			a.opts.Progress.RecordTaskOutcome(workItem, terminalProgressPhase(err))
		}
		err = fmt.Errorf("wait for container analysis Task %s: %w", taskName, err)
		return ai.UnavailableFailureAnalysisResult(request.TestCase, err), err
	}
	if state.Phase != "Succeeded" {
		taskErr := fmt.Errorf("container analysis Task %s ended %s", taskName, state.Phase)
		stateCtx, stateCancel := context.WithTimeout(ctx, failedTaskStateTimeout)
		defer stateCancel()
		if raw, resultErr := a.waitResult(stateCtx, taskName, workItem); resultErr != nil {
			if IsResultAuthorizationError(resultErr) {
				resultErr = fmt.Errorf("read failed container analysis Task %s result: %w", taskName, resultErr)
				return ai.UnavailableFailureAnalysisResult(request.TestCase, resultErr), resultErr
			}
			log.Printf("Warning: failed to read private state from %s: %v", taskName, resultErr)
		} else {
			identity := analysisruntime.NewContainerStateIdentity(a.opts.Namespace, taskName, taskRequest)
			if delta, stateErr := analysisruntime.ParseEncryptedContainerAnalysisState(raw, a.opts.StateKey, identity); stateErr != nil {
				log.Printf("Warning: failed to parse private state from %s: %v", taskName, stateErr)
			} else {
				a.recordCacheDisposition(workItem, resources.CacheSeedIncluded, delta)
				if stateErr := a.state.MergeTraces(delta); stateErr != nil {
					log.Printf("Warning: failed to merge private traces from %s: %v", taskName, stateErr)
				} else if markErr := MarkContainerAnalysisFailureConsumed(stateCtx, a.kube, resources, state.UID); markErr != nil {
					log.Printf("Warning: failed to mark consumed failure %s: %v", taskName, markErr)
				}
			}
		}
		return ai.UnavailableFailureAnalysisResult(request.TestCase, taskErr), taskErr
	}

	resultCtx, resultCancel := context.WithTimeout(ctx, containerResultReadTimeout)
	defer resultCancel()
	raw, err := a.waitResult(resultCtx, taskName, workItem)
	if err != nil {
		err = fmt.Errorf("read container analysis Task %s result: %w", taskName, err)
		return ai.UnavailableFailureAnalysisResult(request.TestCase, err), err
	}
	result, err := ParseContainerAnalysisResult(raw)
	if err != nil {
		return ai.UnavailableFailureAnalysisResult(request.TestCase, err), err
	}
	identity := analysisruntime.NewContainerStateIdentity(a.opts.Namespace, taskName, taskRequest)
	delta, err := analysisruntime.ParseEncryptedContainerAnalysisState(raw, a.opts.StateKey, identity)
	if err != nil {
		return ai.UnavailableFailureAnalysisResult(request.TestCase, err), err
	}
	if a.opts.Progress != nil && !containerAnalysisCacheHit(delta.Traces) {
		a.opts.Progress.RecordFreshAnalysisCompleted(workItem)
	}
	a.recordCacheDisposition(workItem, resources.CacheSeedIncluded, delta)
	if err := a.state.Merge(delta); err != nil {
		log.Printf("Warning: failed to persist state from %s: %v", taskName, err)
		return result, nil
	}
	a.cleanupConsumedBundle(resources, state)
	return result, nil
}

func (a *ContainerAnalyzer) taskSpec(request ai.FailureAnalysisRequest, cacheSeed map[string]ai.CacheEntry, taskLabels map[string]string) ContainerAnalysisTaskSpec {
	secretEnv := []SecretEnvVar{
		{Name: "AI_TOKEN", SecretName: a.opts.ModelSecretName, SecretKey: a.opts.ModelTokenKey},
		{Name: analysisruntime.ContainerStateKeyEnv, SecretName: a.opts.StateSecretName, SecretKey: a.opts.StateSecretKey},
	}
	if a.opts.GitHubSecretName != "" && a.opts.GitHubTokenKey != "" {
		secretEnv = append(secretEnv, SecretEnvVar{
			Name: "GITHUB_READ_TOKEN", SecretName: a.opts.GitHubSecretName, SecretKey: a.opts.GitHubTokenKey,
		})
	}
	return ContainerAnalysisTaskSpec{
		Namespace:           a.opts.Namespace,
		NamePrefix:          "dashboard-analyzer",
		Image:               a.opts.Image,
		Args:                []string{"-data-dir=/tmp/prow-ai-analyzer"},
		Timeout:             a.opts.TaskTimeout.String(),
		MaxRetries:          a.opts.MaxRetries,
		ProjectDir:          a.opts.ProjectDir,
		Request:             request,
		CacheSeed:           cacheSeed,
		StateKeyFingerprint: containerStateKeyFingerprint(a.opts.StateKey),
		Environment:         containerAnalyzerEnvironment(a.opts),
		SecretEnv:           secretEnv,
		Labels:              a.opts.Labels, TaskLabels: taskLabels, NodeSelector: a.opts.NodeSelector,
		Tolerations: a.opts.Tolerations, Affinity: a.opts.Affinity,
	}
}

func (a *ContainerAnalyzer) recordCacheDisposition(workItem string, cacheSeedIncluded bool, delta analysisruntime.ContainerAnalysisState) {
	if a.opts.Progress == nil || !cacheSeedIncluded {
		return
	}
	a.opts.Progress.RecordCacheDisposition(workItem, containerAnalysisCacheHit(delta.Traces))
}

func containerAnalyzerEnvironment(opts ContainerAnalyzerOptions) map[string]string {
	environment := map[string]string{"AI_API": opts.API, "AI_ENDPOINT": opts.Endpoint, "AI_MODEL": opts.Model}
	if opts.CacheGeneration != "" {
		environment[project.AICacheGenerationEnv] = opts.CacheGeneration
	}
	if opts.ContextWindowTokens > 0 {
		environment["AI_CONTEXT_WINDOW_TOKENS"] = fmt.Sprintf("%d", opts.ContextWindowTokens)
	}
	return environment
}

func containerTaskWaitTimeout(taskTimeout time.Duration, retries int) time.Duration {
	const maxDuration = time.Duration(1<<63 - 1)
	total := containerTaskSchedulingGrace
	if retries > 0 {
		if time.Duration(retries) > (maxDuration-total)/containerTaskRetryDelay {
			return maxDuration
		}
		total += time.Duration(retries) * containerTaskRetryDelay
	}
	attempts := time.Duration(retries + 1)
	if taskTimeout > (maxDuration-total)/attempts {
		return maxDuration
	}
	return total + attempts*taskTimeout
}

func (a *ContainerAnalyzer) waitTerminal(ctx context.Context, taskName, workItem string, adopted bool) (TaskState, error) {
	ticker := time.NewTicker(a.opts.PollInterval)
	defer ticker.Stop()
	for {
		state, err := a.kube.TaskState(ctx, a.opts.Namespace, taskName)
		if err != nil {
			return TaskState{}, err
		}
		if a.opts.Progress != nil && state.Exists {
			a.opts.Progress.RecordTaskState(workItem, state.Phase, state.Attempts, adopted)
		}
		if state.Exists && TerminalPhase(state.Phase) {
			return state, nil
		}
		select {
		case <-ctx.Done():
			return TaskState{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *ContainerAnalyzer) waitResult(ctx context.Context, taskName, workItem string) (string, error) {
	ticker := time.NewTicker(a.opts.PollInterval)
	defer ticker.Stop()
	calls := 0
	for {
		raw, ok, err := a.results.Result(ctx, a.opts.Namespace, taskName)
		if a.opts.Progress != nil {
			a.opts.Progress.RecordResultAttempt(workItem, calls > 0, ok && err == nil)
		}
		calls++
		if err != nil {
			return "", err
		}
		if ok {
			return raw, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *ContainerAnalyzer) cleanupConsumedBundle(resources ContainerAnalysisResources, state TaskState) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := CleanupContainerAnalysisBundle(ctx, a.kube, resources, state.UID); err != nil {
		log.Printf("Warning: container analysis bundle cleanup failed: %v", err)
	}
}

// StateStore exposes the shared cache and trace store for final pattern analysis.
func (a *ContainerAnalyzer) StateStore() *analysisruntime.ContainerStateStore {
	if a == nil {
		return nil
	}
	return a.state
}

var _ ai.FailureAnalyzer = (*ContainerAnalyzer)(nil)
