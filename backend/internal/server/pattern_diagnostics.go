package server

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/auth"
)

func patternDiagnosticsHandler(dataDir string, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		auth.SetPrivateResponseHeaders(w.Header())
		if now == nil {
			now = time.Now
		}
		snapshot, err := ai.ReadPatternFailureDiagnostics(filepath.Join(dataDir, ai.CacheFilename), now())
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "pattern diagnostics unavailable", http.StatusNotFound)
				return
			}
			log.Printf("pattern diagnostics: %v", err)
			http.Error(w, "pattern diagnostics unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snapshot)
	})
}
