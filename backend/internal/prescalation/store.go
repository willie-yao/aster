package prescalation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/willie-yao/aster/backend/internal/statefile"
)

// StateFileName is the private pull request escalation result store. It carries
// operator state rather than dashboard data, so it is never published.
const StateFileName = "pr_escalation_state.json"

// ClusterStateFileName is the equivalent store for shared failure escalations.
// The two are separate files because their records are keyed differently, so a
// shared store would make one kind's results unreadable to the other.
const ClusterStateFileName = "shared_failure_escalation_state.json"

// FileStore persists completed escalations under a data directory. Name is
// required, so two escalation kinds cannot silently collide on one file.
type FileStore[R any] struct {
	Dir  string
	Name string
}

type storeDocument[R any] struct {
	Results map[string]View[R] `json:"results"`
}

func (s FileStore[R]) path() (string, error) {
	if s.Name == "" {
		return "", fmt.Errorf("prescalation: file store name is required")
	}
	return filepath.Join(s.Dir, s.Name), nil
}

// Load restores persisted results. A missing file is an empty set.
func (s FileStore[R]) Load() (map[string]View[R], error) {
	path, err := s.path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]View[R]{}, nil
		}
		return nil, err
	}
	var doc storeDocument[R]
	if err := json.Unmarshal(data, &doc); err != nil {
		// A corrupt store must not block escalation; start from empty.
		return map[string]View[R]{}, nil
	}
	if doc.Results == nil {
		doc.Results = map[string]View[R]{}
	}
	return doc.Results, nil
}

// Save writes results atomically.
func (s FileStore[R]) Save(results map[string]View[R]) error {
	path, err := s.path()
	if err != nil {
		return err
	}
	return statefile.WriteJSON(path, storeDocument[R]{Results: results})
}
