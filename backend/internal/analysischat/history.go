package analysischat

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// DefaultHistoryRetention keeps operator conversations for 180 days of inactivity.
const DefaultHistoryRetention = 180 * 24 * time.Hour

// HistoryQuery filters the shared conversation history.
type HistoryQuery struct {
	JobID  string
	Scope  string
	Cursor string
	Limit  int
}

// SessionSummary identifies a saved conversation without loading its transcript.
type SessionSummary struct {
	ID                string      `json:"id"`
	Analysis          AnalysisRef `json:"analysis"`
	CreatedBy         string      `json:"created_by"`
	CreatedAt         string      `json:"created_at"`
	UpdatedAt         string      `json:"updated_at"`
	HistoryExpiresAt  string      `json:"history_expires_at"`
	Archived          bool        `json:"archived"`
	ReadOnly          bool        `json:"read_only"`
	Title             string      `json:"title"`
	BuildIDs          []string    `json:"build_ids,omitempty"`
	ComparisonBuildID string      `json:"comparison_build_id,omitempty"`
}

// HistoryPage is a bounded page of saved operator conversations.
type HistoryPage struct {
	Sessions   []SessionSummary `json:"sessions"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type historyCursor struct {
	JobID     string `json:"job_id"`
	Scope     string `json:"scope"`
	UpdatedAt string `json:"updated_at"`
	ID        string `json:"id"`
}

// List returns retained conversations, newest activity first, across operators.
func (s *Service) List(query HistoryQuery, owner string) (HistoryPage, error) {
	if normalizeOwner(owner) == "" {
		return HistoryPage{}, fmt.Errorf("%w: owner is required", ErrInvalidRequest)
	}
	query.JobID, query.Scope = strings.TrimSpace(query.JobID), strings.TrimSpace(query.Scope)
	if query.Limit == 0 {
		query.Limit = 20
	}
	if query.Limit < 1 || query.Limit > 100 || len(query.JobID) > maxJobIDBytes ||
		(query.Scope != "" && query.Scope != ScopeTest && query.Scope != ScopePattern && query.Scope != ScopeCause) {
		return HistoryPage{}, fmt.Errorf("%w: invalid history filter or page size", ErrInvalidRequest)
	}
	var after historyCursor
	if query.Cursor != "" {
		data, err := base64.RawURLEncoding.DecodeString(query.Cursor)
		if len(query.Cursor) > 4096 || err != nil || json.Unmarshal(data, &after) != nil || after.ID == "" ||
			after.JobID != query.JobID || after.Scope != query.Scope {
			return HistoryPage{}, fmt.Errorf("%w: invalid history cursor", ErrInvalidRequest)
		}
		if _, err := time.Parse(time.RFC3339, after.UpdatedAt); err != nil {
			return HistoryPage{}, fmt.Errorf("%w: invalid history cursor timestamp", ErrInvalidRequest)
		}
	}
	page := HistoryPage{Sessions: []SessionSummary{}}
	now := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	err := s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		for _, current := range state.Sessions {
			if current == nil || current.Retired || current.HistoryExpiresAt.IsZero() ||
				(query.JobID != "" && current.View.Analysis.JobID != query.JobID) ||
				(query.Scope != "" && current.View.Analysis.Scope != query.Scope) {
				continue
			}
			updated := persistedSessionActivity(current).UTC().Format(time.RFC3339)
			if after.ID != "" && (updated > after.UpdatedAt || updated == after.UpdatedAt && current.View.ID >= after.ID) {
				continue
			}
			title, builds := sessionDescription(current)
			page.Sessions = append(page.Sessions, SessionSummary{
				ID: current.View.ID, Analysis: current.View.Analysis, CreatedBy: current.Owner,
				CreatedAt: current.View.CreatedAt, UpdatedAt: updated,
				HistoryExpiresAt: optionalTimestamp(current.HistoryExpiresAt), Archived: current.Archived,
				ReadOnly: !sessionLive(current, now), Title: title, BuildIDs: builds,
				ComparisonBuildID: persistedCauseComparisonBuildID(current.Resolved.Comparison),
			})
		}
		return changed, nil
	})
	if err != nil {
		return HistoryPage{}, err
	}
	slices.SortFunc(page.Sessions, func(a, b SessionSummary) int {
		if order := strings.Compare(b.UpdatedAt, a.UpdatedAt); order != 0 {
			return order
		}
		return strings.Compare(b.ID, a.ID)
	})
	if len(page.Sessions) > query.Limit {
		page.Sessions = page.Sessions[:query.Limit]
		last := page.Sessions[len(page.Sessions)-1]
		data, _ := json.Marshal(historyCursor{JobID: query.JobID, Scope: query.Scope, UpdatedAt: last.UpdatedAt, ID: last.ID})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	return page, nil
}

// Archive stops new turns without removing the conversation or its evidence.
func (s *Service) Archive(id, owner string) error {
	if normalizeOwner(owner) == "" {
		return fmt.Errorf("%w: owner is required", ErrInvalidRequest)
	}
	ctx, cancel := s.store.context()
	defer cancel()
	return s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, s.opts.Now().UTC())
		current := state.Sessions[strings.TrimSpace(id)]
		if current == nil || current.Retired {
			return changed, ErrSessionNotFound
		}
		if current.Active != nil {
			return changed, ErrSessionBusy
		}
		if current.Archived {
			return changed, nil
		}
		current.Archived = true
		return true, nil
	})
}

// RetainForFix keeps a selected answer when an operator requests a Fix investigation.
func (s *Service) RetainForFix(id, owner, requestID string) error {
	if normalizeOwner(owner) == "" {
		return fmt.Errorf("%w: owner is required", ErrInvalidRequest)
	}
	now := s.opts.Now().UTC()
	ctx, cancel := s.store.context()
	defer cancel()
	return s.store.update(ctx, func(state *persistedState) (bool, error) {
		changed := s.cleanup(state, now)
		current := state.Sessions[strings.TrimSpace(id)]
		if current == nil || current.Retired {
			return changed, ErrSessionNotFound
		}
		if request, ok := current.Requests[requestID]; !ok || request.Status != requestSucceeded {
			return changed, ErrRequestNotFound
		}
		current.HistoryExpiresAt = now.Add(s.opts.HistoryRetention)
		current.View.UpdatedAt = now.Format(time.RFC3339)
		return true, nil
	})
}

func sessionLive(current *persistedSession, now time.Time) bool {
	return current != nil && !current.Retired && !current.Archived && (now.Before(current.ExpiresAt) || current.Active != nil)
}

func sessionDescription(current *persistedSession) (string, []string) {
	title := current.View.Analysis.TestName
	if pattern := current.Resolved.Pattern; pattern != nil {
		title = pattern.SharedRootCause
		if current.View.Analysis.Scope == ScopeCause {
			for _, group := range pattern.CausalGroups {
				if group.ID == current.View.Analysis.CausalGroupID {
					title = group.RootCause
					break
				}
			}
		}
	}
	if strings.TrimSpace(title) == "" {
		title = current.Resolved.TestCase.Name
	}
	var builds []string
	for _, build := range current.Resolved.EvidenceBuilds {
		builds = append(builds, build.BuildID)
	}
	if len(builds) == 0 && current.Resolved.Build.BuildID != "" {
		builds = []string{current.Resolved.Build.BuildID}
	}
	return clampPersistedText(title, 1024), builds
}

// operatorActivity ignores prepared findings and automatic lease cleanup timestamps.
func operatorActivity(current *persistedSession) time.Time {
	if current == nil {
		return time.Time{}
	}
	var latest time.Time
	found := false
	observe := func(value string) {
		if stamp, err := time.Parse(time.RFC3339, value); err == nil && stamp.After(latest) {
			latest = stamp
		}
	}
	for _, request := range current.Requests {
		if !request.Prepared {
			found = true
			observe(request.CreatedAt)
			observe(request.UpdatedAt)
		}
	}
	for _, message := range current.View.Messages {
		if message.Role == "user" {
			found = true
			observe(message.CreatedAt)
		}
	}
	if found && latest.IsZero() {
		observe(current.View.CreatedAt)
	}
	return latest
}
