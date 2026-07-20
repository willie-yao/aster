package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/issues"
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
	RequestFailed    = "failed"
	RequestConfirmed = "confirmed"
	RequestCancelled = "cancelled"
	RequestExpired   = "expired"
)

var ErrRequestNotFound = errors.New("action request not found")

// RequestReadyNotifier sends a draft-ready notification after async generation.
type RequestReadyNotifier func(context.Context, ActionRequestView) error

// ActionRequestView is the API-safe representation of a persisted request.
type ActionRequestView struct {
	ID           string         `json:"id"`
	FailureID    string         `json:"failure_id"`
	Kind         string         `json:"kind"`
	Owner        string         `json:"owner"`
	Status       string         `json:"status"`
	CreatedAt    string         `json:"created_at"`
	UpdatedAt    string         `json:"updated_at"`
	ExpiresAt    string         `json:"expires_at"`
	Error        string         `json:"error,omitempty"`
	ResultURL    string         `json:"result_url,omitempty"`
	SupersededBy string         `json:"superseded_by,omitempty"`
	Preview      *PreviewResult `json:"preview,omitempty"`
	EmailSent    bool           `json:"email_sent,omitempty"`
	EmailError   string         `json:"email_error,omitempty"`
}

type actionRequest struct {
	ActionRequestView
	Instruction string                      `json:"instruction,omitempty"`
	Issue       *issues.IssueSpec           `json:"issue,omitempty"`
	Fix         *fixpr.GeneratedFixSnapshot `json:"fix,omitempty"`
}

type actionRequestState struct {
	Version  int                       `json:"version"`
	Requests map[string]*actionRequest `json:"requests"`
}

func (s *Service) requestStatePath() string {
	return filepath.Join(s.dataDir, "action_request_state.json")
}

func (s *Service) loadActionRequests() {
	state := &actionRequestState{Version: 1, Requests: map[string]*actionRequest{}}
	data, err := os.ReadFile(s.requestStatePath())
	if err == nil {
		if err := json.Unmarshal(data, state); err != nil {
			log.Printf("Warning: failed to parse action request state: %v", err)
			state = &actionRequestState{Version: 1, Requests: map[string]*actionRequest{}}
		}
	}
	if state.Requests == nil {
		state.Requests = map[string]*actionRequest{}
	}
	now := time.Now().UTC()
	s.requests = state
	changed := s.expireRequestsLocked(now)
	nowText := now.Format(time.RFC3339)
	for _, request := range state.Requests {
		if request.Status == RequestPending {
			request.Status = RequestFailed
			request.Error = "server restarted before draft generation completed"
			request.UpdatedAt = nowText
			changed = true
		}
	}
	if changed {
		if err := statefile.WriteJSON(s.requestStatePath(), state); err != nil {
			log.Printf("Warning: failed to save recovered action request state: %v", err)
		}
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
	if _, err := s.findPattern(failureID); err != nil {
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
	pending := 0
	active := 0
	for existingID, existing := range s.requests.Requests {
		if existingID == supersedesID {
			continue
		}
		if existing.Status == RequestPending && existing.Owner == owner {
			pending++
		}
		if existing.Status == RequestPending || existing.Status == RequestReady {
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

type requestPreviewGenerator func(context.Context, string, string, string, string) (PreviewResult, *previewEntry, error)

func (s *Service) generateRequest(id, userToken string) {
	s.generateRequestWith(id, userToken, s.generateRequestPreview)
}

func (s *Service) generateRequestPreview(ctx context.Context, failureID, kind, userToken, instruction string) (PreviewResult, *previewEntry, error) {
	switch kind {
	case "create-issue":
		return s.generateIssuePreview(ctx, failureID, userToken, instruction)
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
	s.rmu.Unlock()

	preview, entry, err := generate(ctx, failureID, kind, userToken, instruction)

	s.rmu.Lock()
	request = s.requests.Requests[id]
	if request == nil || request.Status != RequestPending {
		s.rmu.Unlock()
		return
	}
	request.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		request.Status = RequestFailed
		request.Error = safeReason(err.Error())
	} else {
		request.Status = RequestReady
		request.Preview = &preview
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
	if notifier == nil {
		return
	}
	var notifyErr error
	for attempt := 0; attempt < 3; attempt++ {
		notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 30*time.Second)
		notifyErr = notifier(notifyCtx, view)
		notifyCancel()
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
	if request.Status != RequestReady {
		status := request.Status
		s.rmu.Unlock()
		return "", fmt.Errorf("action request is %s", status)
	}
	if _, confirming := s.requestConfirms[id]; confirming {
		s.rmu.Unlock()
		return "", fmt.Errorf("action request is being confirmed")
	}
	if request.Preview == nil {
		s.rmu.Unlock()
		return "", fmt.Errorf("action request has no persisted preview")
	}
	entry := &previewEntry{kind: request.Preview.Kind}
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
	s.requestConfirms[id] = struct{}{}
	s.rmu.Unlock()
	defer func() {
		s.rmu.Lock()
		delete(s.requestConfirms, id)
		s.rmu.Unlock()
	}()

	url, err := s.confirmEntry(ctx, entry, userToken)
	if err != nil {
		return "", err
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
			if request.Error != "" || request.Preview != nil || request.Instruction != "" || request.Issue != nil || request.Fix != nil || request.EmailError != "" {
				request.Error = ""
				request.Preview = nil
				request.Instruction = ""
				request.Issue = nil
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

func (s *Service) saveRequestsLocked() error {
	if err := statefile.WriteJSON(s.requestStatePath(), s.requests); err != nil {
		return fmt.Errorf("saving action request state: %w", err)
	}
	return nil
}
