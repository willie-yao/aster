package fetchprogress

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// HistorySchemaVersion is the current private pass history schema.
	HistorySchemaVersion = 1
	// HistoryFilename stores bounded terminal pass summaries.
	HistoryFilename = "history.json"
	// HistoryLimit bounds retained pass summaries.
	HistoryLimit = 20
)

// PassSummary is one safe terminal fetch-pass summary.
type PassSummary struct {
	RunID            string           `json:"run_id"`
	PassID           string           `json:"pass_id"`
	PassType         PassType         `json:"pass_type"`
	StartedAt        time.Time        `json:"started_at"`
	CompletedAt      time.Time        `json:"completed_at"`
	PhaseDurationsMS map[string]int64 `json:"phase_durations_ms,omitempty"`
	LogicalCount     int              `json:"logical_count"`
	CacheHits        int              `json:"cache_hits"`
	TaskAttempts     int              `json:"task_attempts"`
	Retries          int              `json:"retries"`
	Outcome          Outcome          `json:"outcome"`
	FailureCategory  FailureCategory  `json:"failure_category,omitempty"`
	Published        bool             `json:"published"`
}

// History is the bounded private pass history file.
type History struct {
	SchemaVersion int           `json:"schema_version"`
	Passes        []PassSummary `json:"passes"`
}

// HistoryPath returns the private pass history path for a data directory.
func HistoryPath(dataDir string) string {
	return filepath.Join(dataDir, StatusDirectory, HistoryFilename)
}

// ReadHistory loads and validates bounded pass history.
func ReadHistory(path string) (History, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return History{}, err
	}
	var history History
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&history); err != nil {
		return History{}, fmt.Errorf("decode fetch history: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return History{}, errors.New("fetch history has trailing data")
	}
	if history.SchemaVersion != HistorySchemaVersion {
		return History{}, fmt.Errorf("unsupported fetch history schema %d", history.SchemaVersion)
	}
	if len(history.Passes) > HistoryLimit {
		return History{}, errors.New("fetch history exceeds retention limit")
	}
	for _, summary := range history.Passes {
		if summary.RunID == "" || summary.PassID == "" || !validPassType(summary.PassType) ||
			!validOutcome(summary.Outcome) || summary.Outcome == OutcomeRunning || !validFailureCategory(summary.FailureCategory) ||
			summary.StartedAt.IsZero() || summary.CompletedAt.IsZero() || summary.CompletedAt.Before(summary.StartedAt) ||
			summary.LogicalCount < 0 || summary.CacheHits < 0 || summary.TaskAttempts < 0 || summary.Retries < 0 {
			return History{}, errors.New("fetch history has invalid pass summary")
		}
		for phase, duration := range summary.PhaseDurationsMS {
			if !validPhase(Phase(phase)) || duration < 0 {
				return History{}, errors.New("fetch history has invalid phase duration")
			}
		}
	}
	return history, nil
}

// WriteHistory atomically writes bounded private pass history.
func WriteHistory(path string, history History) error {
	if len(history.Passes) > HistoryLimit {
		history.Passes = append([]PassSummary(nil), history.Passes[len(history.Passes)-HistoryLimit:]...)
	}
	return writePrivateJSON(path, history)
}

func (t *Tracker) appendHistoryLocked(now time.Time) {
	if t.status.PassID == "" || t.status.Outcome == OutcomeRunning {
		return
	}
	for _, existing := range t.history.Passes {
		if existing.PassID == t.status.PassID {
			return
		}
	}
	durations := make(map[string]int64, len(t.status.PhaseDurationsMS))
	for phase, duration := range t.status.PhaseDurationsMS {
		durations[phase] = duration
	}
	summary := PassSummary{
		RunID: t.status.RunID, PassID: t.status.PassID, PassType: t.status.PassType,
		StartedAt: t.status.PassStartedAt, CompletedAt: now, PhaseDurationsMS: durations,
		LogicalCount: t.status.Analyses.LogicalTotal,
		CacheHits:    t.status.Analyses.AcceptedCacheHits,
		TaskAttempts: t.status.Analyses.TaskAttempts,
		Retries:      t.status.Analyses.Retries,
		Outcome:      t.status.Outcome, FailureCategory: t.status.FailureCategory,
		Published: t.publishedThisPass || publicationInPass(t.status),
	}
	t.history.SchemaVersion = HistorySchemaVersion
	t.history.Passes = append(t.history.Passes, summary)
	if len(t.history.Passes) > HistoryLimit {
		t.history.Passes = append([]PassSummary(nil), t.history.Passes[len(t.history.Passes)-HistoryLimit:]...)
	}
	if err := t.writeHistory(HistoryPath(filepath.Dir(filepath.Dir(t.path))), t.history); err != nil {
		t.logPersistenceFailure("history", err)
	}
}

func publicationInPass(status Status) bool {
	return status.LastSuccessfulPublicationAt != nil && !status.LastSuccessfulPublicationAt.Before(status.PassStartedAt)
}
