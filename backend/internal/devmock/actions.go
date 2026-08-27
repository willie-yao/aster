package devmock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/willie-yao/aster/backend/internal/actions"
	"github.com/willie-yao/aster/backend/internal/project"
)

// requestTTL bounds a mock draft, matching the expiry semantics the request UI
// renders.
const requestTTL = time.Hour

// The action names the request routes address, which are distinct from the
// "issue" and "fix" kinds a preview result reports.
const (
	actionIssue = "create-issue"
	actionFix   = "propose-fix"
)

// previewKind maps a request action to the kind its draft reports.
func previewKind(action string) string {
	if action == actionFix {
		return "fix"
	}
	return "issue"
}

// Actions fabricates issue and fix drafting while delegating resolution to the
// real action service, which needs neither a model provider nor GitHub: it
// reads the published patterns out of the data directory and writes
// resolved.json. Resolution therefore behaves exactly as deployed, including
// its validation errors.
type Actions struct {
	*actions.Service

	opts Options

	mu       sync.Mutex
	previews map[string]pendingPreview
	requests map[string]*mockRequest
}

type pendingPreview struct {
	result actions.PreviewResult
}

// mockRequest is one asynchronous draft. Its status is derived from the clock
// rather than a background goroutine, so a poll is always consistent with the
// time it was made and nothing has to be cancelled on shutdown.
type mockRequest struct {
	view         actions.ActionRequestView
	readyAt      time.Time
	terminal     bool
	previewToken string
}

func newActions(cfg *project.Config, opts Options) *Actions {
	return &Actions{
		Service:  actions.NewService(cfg, opts.DataDir, actions.AIConfig{}),
		opts:     opts,
		previews: map[string]pendingPreview{},
		requests: map[string]*mockRequest{},
	}
}

// PreviewIssue drafts an issue for a failure after a simulated model call.
func (a *Actions) PreviewIssue(ctx context.Context, failureID, _, _, instruction string) (actions.PreviewResult, error) {
	return a.preview(ctx, "issue", failureID, instruction)
}

// PreviewFix drafts a fix for a failure after a simulated model call.
func (a *Actions) PreviewFix(ctx context.Context, failureID, _, _, instruction string) (actions.PreviewResult, error) {
	return a.preview(ctx, "fix", failureID, instruction)
}

func (a *Actions) preview(ctx context.Context, kind, failureID, instruction string) (actions.PreviewResult, error) {
	if strings.TrimSpace(failureID) == "" {
		return actions.PreviewResult{}, actions.ErrNotFound
	}
	if err := a.simulate(ctx); err != nil {
		return actions.PreviewResult{}, err
	}
	result := previewFor(kind, failureID, instruction, a.analysisForFailure(failureID))
	result.Token = newID()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.previews[result.Token] = pendingPreview{result: result}
	return result, nil
}

// Confirm reports where a previewed draft would have been published.
func (a *Actions) Confirm(ctx context.Context, token, _, _ string) (string, error) {
	a.mu.Lock()
	preview, ok := a.previews[token]
	delete(a.previews, token)
	a.mu.Unlock()
	if !ok {
		return "", actions.ErrPreviewNotFound
	}
	if err := a.simulate(ctx); err != nil {
		return "", err
	}
	return mockURL(preview.result.Kind, token), nil
}

// ActionEligibility reports every failure as actionable so the action controls
// render. The real preflight verifies pinned source over the network.
func (a *Actions) ActionEligibility(_ context.Context, failureID string) (actions.Eligibility, error) {
	if strings.TrimSpace(failureID) == "" {
		return actions.Eligibility{}, actions.ErrNotFound
	}
	return actions.Eligibility{
		State:  "available",
		Code:   actions.ReasonActionable,
		Reason: "The mock server treats every failure as actionable.",
	}, nil
}

// CreateRequest starts an asynchronous draft. supersedesID cancels an earlier
// request and records the replacement, as the real service does.
func (a *Actions) CreateRequest(failureID, kind, login, _, instruction, supersedesID string) (actions.ActionRequestView, error) {
	if strings.TrimSpace(failureID) == "" {
		return actions.ActionRequestView{}, actions.ErrNotFound
	}
	if kind != actionIssue && kind != actionFix {
		return actions.ActionRequestView{}, fmt.Errorf("unsupported action %q", kind)
	}
	now := a.opts.now()
	request := &mockRequest{
		view: actions.ActionRequestView{
			ID: newID(), FailureID: failureID, Kind: kind, Owner: strings.ToLower(strings.TrimSpace(login)),
			CreatedAt: timestamp(now), UpdatedAt: timestamp(now),
			ExpiresAt: timestamp(now.Add(requestTTL)),
		},
		readyAt: now.Add(a.opts.latency()),
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if superseded := a.requests[supersedesID]; superseded != nil && !superseded.terminal {
		superseded.view.Status = actions.RequestCancelled
		superseded.view.Stage = ""
		superseded.view.SupersededBy = request.view.ID
		superseded.view.UpdatedAt = timestamp(now)
		superseded.terminal = true
	}
	a.instruction(request, instruction)
	a.requests[request.view.ID] = request
	return a.viewLocked(request, now), nil
}

// instruction records the maintainer's refinement on the draft the request will
// produce, so a refined request differs from the one it replaced.
func (a *Actions) instruction(request *mockRequest, instruction string) {
	request.view.Warning = ""
	if strings.TrimSpace(instruction) != "" {
		request.view.Warning = "Draft refined by a maintainer instruction."
	}
}

// GetRequest returns the current state of one request.
func (a *Actions) GetRequest(id, login string) (actions.ActionRequestView, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	request, err := a.ownedLocked(id, login)
	if err != nil {
		return actions.ActionRequestView{}, err
	}
	return a.viewLocked(request, a.opts.now()), nil
}

// ConfirmRequest publishes a ready draft.
func (a *Actions) ConfirmRequest(_ context.Context, id, login, _ string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	request, err := a.ownedLocked(id, login)
	if err != nil {
		return "", err
	}
	now := a.opts.now()
	view := a.viewLocked(request, now)
	switch view.Status {
	case actions.RequestReady:
	case actions.RequestPending, actions.RequestCancelling:
		return "", actions.ErrPreviewPending
	default:
		return "", actions.ErrPreviewNotFound
	}
	delete(a.previews, request.previewToken)
	request.view = view
	request.view.Status = actions.RequestConfirmed
	request.view.Stage = ""
	request.view.ResultURL = mockURL(previewKind(request.view.Kind), request.view.ID)
	request.view.UpdatedAt = timestamp(now)
	request.terminal = true
	return request.view.ResultURL, nil
}

// CancelRequest abandons a request that has not completed.
func (a *Actions) CancelRequest(_ context.Context, id, login string) (actions.ActionRequestView, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	request, err := a.ownedLocked(id, login)
	if err != nil {
		return actions.ActionRequestView{}, err
	}
	now := a.opts.now()
	if view := a.viewLocked(request, now); view.Status != actions.RequestPending && view.Status != actions.RequestReady {
		return view, nil
	}
	delete(a.previews, request.previewToken)
	request.view.Status = actions.RequestCancelled
	request.view.Stage = ""
	request.view.Preview = nil
	request.view.UpdatedAt = timestamp(now)
	request.terminal = true
	return request.view, nil
}

// createAnalysisFixRequest starts an asynchronous fix draft from a chat answer.
func (a *Actions) createAnalysisFixRequest(failureID, login, instruction string, replaces ...string) (actions.ActionRequestView, error) {
	supersedes := ""
	if len(replaces) > 0 {
		supersedes = replaces[0]
	}
	return a.CreateRequest(failureID, actionFix, login, "", instruction, supersedes)
}

// ownedLocked resolves a request the caller owns.
func (a *Actions) ownedLocked(id, login string) (*mockRequest, error) {
	request, ok := a.requests[id]
	if !ok || !strings.EqualFold(request.view.Owner, login) {
		return nil, actions.ErrRequestNotFound
	}
	return request, nil
}

// viewLocked derives the request's current status from the clock. Generation
// settles once: a ready request keeps the draft and preview token it published,
// so a poll cannot invalidate a token the operator is about to confirm, and
// repeated polls do not accumulate previews.
func (a *Actions) viewLocked(request *mockRequest, now time.Time) actions.ActionRequestView {
	if request.terminal {
		return request.view
	}
	if expires, err := time.Parse(time.RFC3339, request.view.ExpiresAt); err == nil && now.After(expires) {
		delete(a.previews, request.previewToken)
		request.view.Status = actions.RequestExpired
		request.view.Stage = ""
		request.view.Preview = nil
		request.view.UpdatedAt = timestamp(now)
		request.terminal = true
		return request.view
	}
	if request.view.Status == actions.RequestReady {
		return request.view
	}
	if now.Before(request.readyAt) {
		request.view.Status = actions.RequestPending
		request.view.Stage = actions.RequestStageDrafting
		if request.view.Kind == actionFix && now.After(request.readyAt.Add(-a.opts.latency()/2)) {
			request.view.Stage = actions.RequestStageVerifying
		}
		return request.view
	}

	kind := previewKind(request.view.Kind)
	preview := previewFor(kind, request.view.FailureID, instructionFrom(request.view), a.analysisForFailure(request.view.FailureID))
	preview.Token = newID()
	a.previews[preview.Token] = pendingPreview{result: preview}
	request.previewToken = preview.Token
	request.view.Status = actions.RequestReady
	request.view.Stage = ""
	request.view.Preview = &preview
	request.view.UpdatedAt = timestamp(now)
	if kind == "fix" {
		request.view.Verification = &actions.ActionVerificationView{
			State: "available", Code: actions.ReasonActionable,
			Reason: "The mock server does not verify remediations against source.",
		}
	}
	return request.view
}

// instructionFrom recovers whether the request carried a refinement, which is
// all the mock draft needs to differ.
func instructionFrom(view actions.ActionRequestView) string {
	if view.Warning == "" {
		return ""
	}
	return "Refined by a maintainer instruction."
}

// analysisForFailure resolves the published analysis behind a failure id. A
// pattern id addresses a correlated pattern rather than one test, so the mock
// falls back to an empty analysis and still drafts something readable.
func (a *Actions) analysisForFailure(failureID string) publishedAnalysis {
	job, build, test := splitFailureID(failureID)
	return lookupAnalysis(a.opts.DataDir, job, build, test)
}

// splitFailureID reads the job, build, and test out of an analysis-scoped
// failure id. Pattern ids carry none of those and yield empty strings.
func splitFailureID(failureID string) (job, build, test string) {
	parts := strings.SplitN(failureID, "/", 3)
	if len(parts) != 3 {
		return "", "", ""
	}
	return parts[0], parts[1], parts[2]
}

// simulate waits out a fabricated model call, honoring cancellation so a
// client that navigates away is not left waiting.
func (a *Actions) simulate(ctx context.Context) error {
	timer := time.NewTimer(a.opts.latency())
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func newID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("mock-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
