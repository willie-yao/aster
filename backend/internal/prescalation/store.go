package prescalation

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/willie-yao/aster/backend/internal/statefile"
)

// StateFileName is the private escalation result store. It carries operator
// state rather than dashboard data, so it is never published.
const StateFileName = "pr_escalation_state.json"

// FileStore persists completed escalations under a data directory.
type FileStore struct {
	Dir string
}

type storeDocument struct {
	Results map[string]View `json:"results"`
}

// Load restores persisted results. A missing file is an empty set.
func (s FileStore) Load() (map[string]View, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, StateFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]View{}, nil
		}
		return nil, err
	}
	var doc storeDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		// A corrupt store must not block escalation; start from empty.
		return map[string]View{}, nil
	}
	if doc.Results == nil {
		doc.Results = map[string]View{}
	}
	return doc.Results, nil
}

// Save writes results atomically.
func (s FileStore) Save(results map[string]View) error {
	return statefile.WriteJSON(filepath.Join(s.Dir, StateFileName), storeDocument{Results: results})
}
