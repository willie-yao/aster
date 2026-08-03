package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/actiondraft"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/patternstate"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/statefile"
)

const (
	defaultRequestTimeout = 10 * time.Minute
	actionRequestTTL      = 24 * time.Hour
	maxActiveRequests     = 50
	maxPendingPerOwner    = 3
)

// Action request states.
const (
	RequestPending   = "pending"
	RequestReady     = "ready"
	RequestUnknown   = "unknown"
	RequestFailed    = "failed"
	RequestConfirmed = "confirmed"
	RequestCancelled = "cancelled"
	RequestExpired   = "expired"
)

const draftRefinementWarning = "The revised draft could not be generated or did not pass safety validation. The safe fallback draft is shown below, but this replacement request cannot be confirmed."

var ErrRequestNotFound = errors.New("action request not found")

// RequestReadyNotifier sends a draft-ready notification after async generation.
type RequestReadyNotifier func(context.Context, ActionRequestView) error

// ActionRequestView is the API-safe representation of a persisted request.
type ActionRequestView struct {
	ID           string         `json:"id"`
	FailureID    string         `json:"failure_id"`
	PatternHash  string         `json:"pattern_hash,omitempty"`
	Kind         string         `json:"kind"`
	Owner        string         `json:"owner"`
	Status       string         `json:"status"`
	CreatedAt    string         `json:"created_at"`
	UpdatedAt    string         `json:"updated_at"`
	ExpiresAt    string         `json:"expires_at"`
	Error        string         `json:"error,omitempty"`
	Warning      string         `json:"warning,omitempty"`
	ResultURL    string         `json:"result_url,omitempty"`
	SupersededBy string         `json:"superseded_by,omitempty"`
	Preview      *PreviewResult `json:"preview,omitempty"`
	EmailSent    bool           `json:"email_sent,omitempty"`
	EmailError   string         `json:"email_error,omitempty"`
}

type actionRequest struct {
	ActionRequestView
	Instruction     string                      `json:"instruction,omitempty"`
	Issue           *issues.IssueSpec           `json:"issue,omitempty"`
	Fix             *fixpr.GeneratedFixSnapshot `json:"fix,omitempty"`
	TargetRepo      string                      `json:"target_repo,omitempty"`
	TargetConfig    string                      `json:"target_config,omitempty"`
	BaseIssue       *issues.IssueSpec           `json:"base_issue,omitempty"`
	BaseTargetRepo  string                      `json:"base_target_repo,omitempty"`
	BasePatternHash string                      `json:"base_pattern_hash,omitempty"`
}

type actionRequestState struct {
	Version  int                       `json:"version"`
	Requests map[string]*actionRequest `json:"requests"`
}

func (s *Service) requestStatePath() string {
	return filepath.Join(s.dataDir, "action_request_state.json")
}

func (s *Service) loadActionRequests() {
	state := &actionRequestState{Version: 2, Requests: map[string]*actionRequest{}}
	data, err := os.ReadFile(s.requestStatePath())
	if err == nil {
		if err := json.Unmarshal(data, state); err != nil {
			log.Printf("Warning: failed to parse action request state: %v", err)
			state = &actionRequestState{Version: 2, Requests: map[string]*actionRequest{}}
		}
	}
	if state.Version == 1 {
		for _, request := range state.Requests {
			if request != nil && request.Status == RequestReady && request.PatternHash == "" {
				request.Status = RequestFailed
				request.Error = "pattern changed before confirmation"
			}
		}
		state.Version = 2
	}
	if state.Requests == nil {
		state.Requests = map[string]*actionRequest{}
	}
	now := time.Now().UTC()
	s.requests = state
	changed := s.expireRequestsLocked(now)
	nowText := now.Format(time.RFC3339)
	for _, request := range state.Requests {
		if request.Status != RequestReady && request.Status != RequestUnknown {
			continue
		}
		preview, err := validatedReadyPreview(request)
		if err != nil {
			if request.Status == RequestUnknown {
				if request.Preview != nil {
					request.Preview = nil
					changed = true
				}
				continue
			}
			request.Status = RequestFailed
			request.Error = "saved draft did not pass current safety validation"
			request.Warning = ""
			request.Preview = nil
			request.Issue = nil
			request.Fix = nil
			request.Instruction = ""
			request.UpdatedAt = nowText
			changed = true
			continue
		}
		if !reflect.DeepEqual(request.Preview, preview) {
			request.Preview = preview
			changed = true
		}
	}
	for _, request := range state.Requests {
		if request.Status != RequestPending {
			continue
		}
		request.Status = RequestFailed
		if request.BaseIssue != nil {
			entry := &previewEntry{kind: "issue", spec: *request.BaseIssue}
			if preview, err := validatedPreviewEntry(entry); err == nil {
				request.Warning = draftRefinementWarning
				request.Preview = &preview
			} else {
				request.Error = "saved fallback draft did not pass current safety validation"
			}
		} else {
			request.Error = "server restarted before draft generation completed"
		}
		request.BaseIssue = nil
		request.BaseTargetRepo = ""
		request.BasePatternHash = ""
		request.UpdatedAt = nowText
		changed = true
	}
	if changed {
		if err := statefile.WriteJSON(s.requestStatePath(), state); err != nil {
			log.Printf("Warning: failed to save recovered action request state: %v", err)
		}
	}
}

func validatedReadyPreview(request *actionRequest) (*PreviewResult, error) {
	entry := &previewEntry{kind: request.Kind}
	switch request.Kind {
	case "create-issue":
		if request.Issue == nil {
			return nil, fmt.Errorf("ready issue request has no issue draft")
		}
		entry.kind = "issue"
		entry.spec = *request.Issue
	case "propose-fix":
		if request.Fix == nil {
			return nil, fmt.Errorf("ready fix request has no fix draft")
		}
		entry.kind = gfKind
		entry.fix = fixpr.RestoreGeneratedFix(request.Fix)
	default:
		return nil, fmt.Errorf("ready request has unsupported action %q", request.Kind)
	}
	preview, err := validatedPreviewEntry(entry)
	if err != nil {
		return nil, err
	}
	return &preview, nil
}

func validatedPreviewEntry(entry *previewEntry) (PreviewResult, error) {
	if entry == nil {
		return PreviewResult{}, fmt.Errorf("preview entry is missing")
	}
	switch entry.kind {
	case "issue":
		if strings.TrimSpace(entry.spec.Key) == "" {
			return PreviewResult{}, fmt.Errorf("issue preview key is missing")
		}
		body := strings.ReplaceAll(entry.spec.Body, issues.MarkerFor(entry.spec.Key), "")
		if err := actiondraft.ValidateTitleBody(entry.spec.Title, body); err != nil {
			return PreviewResult{}, err
		}
		return PreviewResult{Kind: "issue", Title: entry.spec.Title, Body: entry.spec.Body}, nil
	case gfKind:
		if entry.fix == nil {
			return PreviewResult{}, fmt.Errorf("fix preview is missing")
		}
		if err := actiondraft.ValidateTitleBody(entry.fix.Title, entry.fix.Description); err != nil {
			return PreviewResult{}, err
		}
		return PreviewResult{
			Kind: gfKind, Title: entry.fix.Title, Body: entry.fix.Description, Diff: entry.fix.Preview.Diff,
			VerifyStatus: string(entry.fix.Preview.Verify.Status), VerifySummary: entry.fix.Preview.Verify.Summary, VerifyOutput: entry.fix.Preview.Verify.Output,
		}, nil
	default:
		return PreviewResult{}, fmt.Errorf("preview kind %q is unsupported", entry.kind)
	}
}

// ConfigureAsyncRequests sets the generation timeout and draft-ready notifier.
func (s *Service) ConfigureAsyncRequests(timeout time.Duration, notifier RequestReadyNotifier) {
	if timeout > 0 {
		s.requestTimeout = timeout
	}
	s.requestNotify = notifier
	if notifier == nil {
		return
	}
	s.rmu.Lock()
	changed := s.expireRequestsLocked(time.Now().UTC())
	var pending []ActionRequestView
	for _, request := range s.requests.Requests {
		if request.Status == RequestReady && !request.EmailSent {
			pending = append(pending, request.ActionRequestView)
		}
	}
	if changed {
		if err := s.saveRequestsLocked(); err != nil {
			log.Printf("Warning: failed to save expired action requests: %v", err)
		}
	}
	s.rmu.Unlock()
	for _, request := range pending {
		go s.notifyRequestReady(request)
	}
}

// CreateRequest persists a pending request and starts draft generation.
func (s *Service) CreateRequest(failureID, kind, owner, userToken, instruction, supersedesID string) (ActionRequestView, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	if owner == "" || userToken == "" {
		return ActionRequestView{}, fmt.Errorf("authenticated owner and token are required")
	}
	if kind != "create-issue" && kind != "propose-fix" {
		return ActionRequestView{}, fmt.Errorf("unsupported action %q", kind)
	}
	subject, err := s.resolveSubject(failureID)
	if err != nil {
		return ActionRequestView{}, err
	}

	id, err := newToken()
	if err != nil {
		return ActionRequestView{}, fmt.Errorf("creating action request id: %w", err)
	}
	now := time.Now().UTC()
	request := &actionRequest{ActionRequestView: ActionRequestView{
		ID: id, FailureID: failureID, Kind: kind, Owner: owner,
		Status: RequestPending, CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(actionRequestTTL).Format(time.RFC3339),
	}, Instruction: strings.TrimSpace(instruction)}
	supersedesID = strings.TrimSpace(supersedesID)

	s.rmu.Lock()
	s.expireRequestsLocked(now)
	var superseded *actionRequest
	var supersededStatus, supersededUpdatedAt, supersededBy string
	var supersededCancel context.CancelFunc
	if supersedesID != "" {
		superseded = s.requests.Requests[supersedesID]
		if superseded == nil || superseded.Owner != owner {
			s.rmu.Unlock()
			return ActionRequestView{}, ErrRequestNotFound
		}
		if superseded.FailureID != failureID {
			s.rmu.Unlock()
			return ActionRequestView{}, fmt.Errorf("superseded action request does not match failure")
		}
		if _, confirming := s.requestConfirms[supersedesID]; confirming {
			s.rmu.Unlock()
			return ActionRequestView{}, fmt.Errorf("action request is being confirmed")
		}
		if superseded.Status != RequestPending && superseded.Status != RequestReady {
			status := superseded.Status
			s.rmu.Unlock()
			return ActionRequestView{}, fmt.Errorf("action request is %s", status)
		}
	}
	for existingID, existing := range s.requests.Requests {
		if existingID == supersedesID || existing.Status != RequestUnknown || existing.FailureID != failureID || existing.Kind != kind {
			continue
		}
		if existing.Owner == owner {
			view := existing.ActionRequestView
			s.rmu.Unlock()
			return view, nil
		}
		s.rmu.Unlock()
		return ActionRequestView{}, fmt.Errorf("an existing action for this failure has an unknown GitHub outcome")
	}
	pending := 0
	active := 0
	for existingID, existing := range s.requests.Requests {
		if existingID == supersedesID {
			continue
		}
		if existing.Status == RequestPending && existing.Owner == owner {
			pending++
		}
		if existing.Status == RequestPending || existing.Status == RequestReady || existing.Status == RequestUnknown {
			active++
		}
	}
	if pending >= maxPendingPerOwner {
		s.rmu.Unlock()
		return ActionRequestView{}, fmt.Errorf("too many pending action requests")
	}
	if active >= maxActiveRequests {
		s.rmu.Unlock()
		return ActionRequestView{}, fmt.Errorf("too many active action requests")
	}
	if superseded != nil {
		if request.Instruction != "" && kind == "create-issue" && superseded.Kind == kind && superseded.Status == RequestReady && superseded.Issue != nil {
			if superseded.PatternHash == "" || superseded.PatternHash != subject.ContentHash {
				s.rmu.Unlock()
				return ActionRequestView{}, ErrPreviewTargetChanged
			}
			base := *superseded.Issue
			base.Labels = slices.Clone(base.Labels)
			request.BaseIssue = &base
			request.BaseTargetRepo = superseded.TargetRepo
			request.BasePatternHash = superseded.PatternHash
		}
		supersededStatus = superseded.Status
		supersededUpdatedAt = superseded.UpdatedAt
		supersededBy = superseded.SupersededBy
		superseded.Status = RequestCancelled
		superseded.UpdatedAt = now.Format(time.RFC3339)
		superseded.SupersededBy = request.ID
		supersededCancel = s.requestCancels[supersedesID]
	}
	s.requests.Requests[request.ID] = request
	if err := s.saveRequestsLocked(); err != nil {
		delete(s.requests.Requests, request.ID)
		if superseded != nil {
			superseded.Status = supersededStatus
			superseded.UpdatedAt = supersededUpdatedAt
			superseded.SupersededBy = supersededBy
		}
		s.rmu.Unlock()
		return ActionRequestView{}, err
	}
	view := request.ActionRequestView
	s.rmu.Unlock()

	if supersededCancel != nil {
		supersededCancel()
	}
	go s.generateRequest(request.ID, userToken)
	return view, nil
}

type requestPreviewGenerator func(context.Context, string, string, string, string, *issues.IssueSpec, string, string) (PreviewResult, *previewEntry, error)

func (s *Service) generateRequest(id, userToken string) {
	s.generateRequestWith(id, userToken, s.generateRequestPreview)
}

func (s *Service) generateRequestPreview(ctx context.Context, failureID, kind, userToken, instruction string, baseIssue *issues.IssueSpec, baseTargetRepo, basePatternHash string) (PreviewResult, *previewEntry, error) {
	switch kind {
	case "create-issue":
		return s.generateIssuePreview(ctx, failureID, userToken, instruction, baseIssue, baseTargetRepo, basePatternHash)
	case "propose-fix":
		return s.generateFixPreview(ctx, failureID, userToken, instruction)
	default:
		return PreviewResult{}, nil, fmt.Errorf("unsupported action %q", kind)
	}
}

func (s *Service) generateRequestWith(id, userToken string, generate requestPreviewGenerator) {
	ctx, cancel := context.WithTimeout(context.Background(), s.requestTimeout)
	s.rmu.Lock()
	s.requestCancels[id] = cancel
	s.rmu.Unlock()
	defer func() {
		cancel()
		s.rmu.Lock()
		delete(s.requestCancels, id)
		s.rmu.Unlock()
	}()

	s.rmu.Lock()
	request := s.requests.Requests[id]
	if request == nil || request.Status != RequestPending {
		s.rmu.Unlock()
		return
	}
	failureID, kind, instruction := request.FailureID, request.Kind, request.Instruction
	baseTargetRepo := request.BaseTargetRepo
	basePatternHash := request.BasePatternHash
	var baseIssue *issues.IssueSpec
	if request.BaseIssue != nil {
		base := *request.BaseIssue
		base.Labels = slices.Clone(base.Labels)
		baseIssue = &base
	}
	s.rmu.Unlock()

	preview, entry, err := generate(ctx, failureID, kind, userToken, instruction, baseIssue, baseTargetRepo, basePatternHash)
	fallbackPreview := errors.Is(err, ErrDraftRefinementRejected) && entry != nil
	if err == nil || fallbackPreview {
		if validateErr := s.validateSubjectSnapshot(failureID, entry.patternHash, entry.kind); validateErr != nil {
			err = validateErr
			fallbackPreview = false
		} else if validated, validateErr := validatedPreviewEntry(entry); validateErr != nil {
			err = fmt.Errorf("%w: generated draft did not pass safety validation", ErrPreviewRejected)
			fallbackPreview = false
		} else {
			preview = validated
		}
	}

	s.rmu.Lock()
	request = s.requests.Requests[id]
	if request == nil || request.Status != RequestPending {
		s.rmu.Unlock()
		return
	}
	request.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	request.BaseIssue = nil
	request.BaseTargetRepo = ""
	request.BasePatternHash = ""
	if err != nil {
		request.Status = RequestFailed
		if fallbackPreview {
			request.Warning = draftRefinementWarning
			request.Preview = &preview
		} else {
			request.Error = safeReason(err.Error())
		}
	} else {
		request.Status = RequestReady
		request.Preview = &preview
		request.TargetRepo = entry.targetRepo
		request.TargetConfig = entry.targetConfig
		request.PatternHash = entry.patternHash
		if entry.kind == "issue" {
			spec := entry.spec
			request.Issue = &spec
		} else {
			request.Fix = entry.fix.Snapshot()
		}
	}
	saveErr := s.saveRequestsLocked()
	view := request.ActionRequestView
	notifier := s.requestNotify
	s.rmu.Unlock()
	if saveErr != nil {
		log.Printf("action request %s: save result: %v", id, saveErr)
		return
	}
	if err != nil || notifier == nil {
		return
	}
	s.notifyRequestReady(view)
}

func (s *Service) notifyRequestReady(view ActionRequestView) {
	s.rmu.Lock()
	changed := s.expireRequestsLocked(time.Now().UTC())
	if changed {
		if err := s.saveRequestsLocked(); err != nil {
			log.Printf("Warning: failed to save expired action requests: %v", err)
		}
	}
	current := s.requests.Requests[view.ID]
	if current == nil || current.Status != RequestReady || current.EmailSent {
		s.rmu.Unlock()
		return
	}
	view = current.ActionRequestView
	notifier := s.requestNotify
	s.rmu.Unlock()
	if notifier == nil || s.validateSubjectSnapshot(view.FailureID, view.PatternHash, view.Kind) != nil {
		return
	}
	var notifyErr error
	for attempt := 0; attempt < 3; attempt++ {
		if s.validateSubjectSnapshot(view.FailureID, view.PatternHash, view.Kind) != nil {
			return
		}
		notifyErr = patternstate.WithLock(s.dataDir, func() error {
			if err := s.validateSubjectSnapshot(view.FailureID, view.PatternHash, view.Kind); err != nil {
				return err
			}
			notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer notifyCancel()
			return notifier(notifyCtx, view)
		})
		if notifyErr == nil {
			break
		}
		if attempt < 2 {
			time.Sleep(time.Duration(1+attempt*2) * time.Second)
		}
	}
	s.rmu.Lock()
	if current := s.requests.Requests[view.ID]; current != nil && current.Status == RequestReady {
		current.EmailSent = notifyErr == nil
		if notifyErr != nil {
			current.EmailError = notifyErr.Error()
		} else {
			current.EmailError = ""
		}
		current.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := s.saveRequestsLocked(); err != nil {
			log.Printf("action request %s: save notification status: %v", view.ID, err)
		}
	}
	s.rmu.Unlock()
}

// GetRequest returns one request only to its owning admin.
func (s *Service) GetRequest(id, owner string) (ActionRequestView, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.expireRequestsLocked(time.Now().UTC()) {
		if err := s.saveRequestsLocked(); err != nil {
			return ActionRequestView{}, err
		}
	}
	request := s.requests.Requests[id]
	if request == nil || request.Owner != strings.ToLower(strings.TrimSpace(owner)) {
		return ActionRequestView{}, ErrRequestNotFound
	}
	return request.ActionRequestView, nil
}

// ConfirmRequest posts the exact persisted draft using the current admin token.
func (s *Service) ConfirmRequest(ctx context.Context, id, owner, userToken string) (string, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	s.rmu.Lock()
	if s.expireRequestsLocked(time.Now().UTC()) {
		if err := s.saveRequestsLocked(); err != nil {
			s.rmu.Unlock()
			return "", err
		}
	}
	request := s.requests.Requests[id]
	if request == nil || request.Owner != owner {
		s.rmu.Unlock()
		return "", ErrRequestNotFound
	}
	if request.Status == RequestConfirmed && request.ResultURL != "" {
		url := request.ResultURL
		s.rmu.Unlock()
		return url, nil
	}
	if request.Status != RequestReady && request.Status != RequestUnknown {
		status := request.Status
		s.rmu.Unlock()
		return "", fmt.Errorf("action request is %s", status)
	}
	reconcileOnly := request.Status == RequestUnknown
	if _, confirming := s.requestConfirms[id]; confirming {
		s.rmu.Unlock()
		return "", fmt.Errorf("action request is being confirmed")
	}
	entryKind := ""
	if request.Preview != nil {
		entryKind = request.Preview.Kind
	} else if reconcileOnly {
		switch request.Kind {
		case "create-issue":
			entryKind = "issue"
		case "propose-fix":
			entryKind = gfKind
		}
	}
	if entryKind == "" {
		s.rmu.Unlock()
		return "", fmt.Errorf("action request has no persisted preview")
	}
	entry := &previewEntry{failureID: request.FailureID, patternHash: request.PatternHash, kind: entryKind, targetRepo: request.TargetRepo, targetConfig: request.TargetConfig}
	switch entry.kind {
	case "issue":
		if request.Issue == nil {
			s.rmu.Unlock()
			return "", fmt.Errorf("action request has no persisted issue draft")
		}
		entry.spec = *request.Issue
	case gfKind:
		if request.Fix == nil {
			s.rmu.Unlock()
			return "", fmt.Errorf("action request has no persisted fix draft")
		}
		entry.fix = fixpr.RestoreGeneratedFix(request.Fix)
	default:
		s.rmu.Unlock()
		return "", fmt.Errorf("action request has invalid preview kind %q", entry.kind)
	}
	if !reconcileOnly && entry.failureID != "" {
		if err := s.validateSubjectSnapshot(entry.failureID, entry.patternHash, entry.kind); err != nil {
			s.rmu.Unlock()
			return "", err
		}
		request.Status = RequestUnknown
		request.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := s.saveRequestsLocked(); err != nil {
			request.Status = RequestReady
			s.rmu.Unlock()
			return "", err
		}
	}
	s.requestConfirms[id] = struct{}{}
	s.rmu.Unlock()
	defer func() {
		s.rmu.Lock()
		delete(s.requestConfirms, id)
		s.rmu.Unlock()
	}()

	var url string
	if reconcileOnly {
		reconciledURL, found, err := s.reconcileEntry(ctx, entry, userToken)
		if err != nil {
			return "", err
		}
		if !found {
			return "", ErrPreviewOutcomeUnknown
		}
		url = reconciledURL
	} else {
		confirmedURL, err := s.confirmEntry(ctx, entry, userToken)
		if errors.Is(err, ErrPreviewOutcomeUnknown) {
			return "", err
		}
		if err != nil {
			s.rmu.Lock()
			if current := s.requests.Requests[id]; current != nil {
				current.Status = RequestReady
				current.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				_ = s.saveRequestsLocked()
			}
			s.rmu.Unlock()
			return "", err
		}
		url = confirmedURL
	}
	s.rmu.Lock()
	if current := s.requests.Requests[id]; current != nil {
		current.Status = RequestConfirmed
		current.ResultURL = url
		current.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := s.saveRequestsLocked(); err != nil {
			s.rmu.Unlock()
			return "", err
		}
	}
	s.rmu.Unlock()
	return url, nil
}

// CancelRequest cancels a pending request or expires a ready draft.
func (s *Service) CancelRequest(id, owner string) error {
	owner = strings.ToLower(strings.TrimSpace(owner))
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.expireRequestsLocked(time.Now().UTC()) {
		if err := s.saveRequestsLocked(); err != nil {
			return err
		}
	}
	request := s.requests.Requests[id]
	if request == nil || request.Owner != owner {
		return ErrRequestNotFound
	}
	if _, confirming := s.requestConfirms[id]; confirming {
		return fmt.Errorf("action request is being confirmed")
	}
	if request.Status != RequestPending && request.Status != RequestReady {
		return fmt.Errorf("action request is %s", request.Status)
	}
	if cancel := s.requestCancels[id]; cancel != nil {
		cancel()
	}
	request.Status = RequestCancelled
	request.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return s.saveRequestsLocked()
}

func (s *Service) expireRequestsLocked(now time.Time) bool {
	changed := false
	for id, request := range s.requests.Requests {
		if _, confirming := s.requestConfirms[id]; confirming {
			continue
		}
		expires, err := time.Parse(time.RFC3339, request.ExpiresAt)
		if err == nil && now.After(expires) {
			if request.Status != RequestExpired {
				request.Status = RequestExpired
				request.UpdatedAt = now.Format(time.RFC3339)
				changed = true
			}
			if request.Error != "" || request.Warning != "" || request.Preview != nil || request.Instruction != "" || request.Issue != nil || request.BaseIssue != nil || request.BaseTargetRepo != "" || request.BasePatternHash != "" || request.Fix != nil || request.EmailError != "" {
				request.Error = ""
				request.Warning = ""
				request.Preview = nil
				request.Instruction = ""
				request.Issue = nil
				request.BaseIssue = nil
				request.BaseTargetRepo = ""
				request.BasePatternHash = ""
				request.Fix = nil
				request.EmailError = ""
				changed = true
			}
		}
	}
	if len(s.requests.Requests) <= 200 {
		return changed
	}
	type item struct{ id, updated string }
	var completed []item
	for id, request := range s.requests.Requests {
		if request.Status != RequestPending && request.Status != RequestReady {
			completed = append(completed, item{id, request.UpdatedAt})
		}
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].updated < completed[j].updated })
	for len(s.requests.Requests) > 200 && len(completed) > 0 {
		delete(s.requests.Requests, completed[0].id)
		completed = completed[1:]
		changed = true
	}
	return changed
}

func (s *Service) validateSubjectSnapshot(failureID, patternHash string, kind ...string) error {
	subject, err := s.resolveSubject(failureID)
	if err != nil {
		return err
	}
	if patternHash == "" || subject.ContentHash != patternHash {
		return ErrPreviewTargetChanged
	}
	fix := len(kind) > 0 && (kind[0] == gfKind || kind[0] == "propose-fix")
	if fix && subject.Kind == actionSubjectBuild {
		eff := s.cfg.EffectiveFixPRs()
		if eff.Repo == nil || len(verifiedBuildSourceFiles(subject.Build, eff.Repo.Owner, eff.Repo.Name)) == 0 {
			return ErrPreviewTargetChanged
		}
	}
	return nil
}

func (s *Service) saveRequestsLocked() error {
	if err := statefile.WriteJSON(s.requestStatePath(), s.requests); err != nil {
		return fmt.Errorf("saving action request state: %w", err)
	}
	return nil
}
