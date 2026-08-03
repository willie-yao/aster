package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/auth"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
)

const maxAnalysisTraceFileBytes = 64 << 20

func analysisTracesHandler(dataDir string, attachment bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.SetPrivateResponseHeaders(w.Header())
		traces, err := readAnalysisTraces(filepath.Join(dataDir, output.AITraceFilename))
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "analysis traces unavailable", http.StatusNotFound)
				return
			}
			log.Printf("analysis traces: %v", err)
			http.Error(w, "analysis traces unavailable", http.StatusInternalServerError)
			return
		}
		traces.Traces = filterAnalysisTraces(traces.Traces, r)
		w.Header().Set("Content-Type", "application/json")
		if attachment {
			w.Header().Set("Content-Disposition", `attachment; filename="analysis-traces.json"`)
		}
		_ = json.NewEncoder(w).Encode(traces)
	})
}

func readAnalysisTraces(path string) (ai.AnalysisTraceFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return ai.AnalysisTraceFile{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxAnalysisTraceFileBytes+1))
	if err != nil {
		return ai.AnalysisTraceFile{}, err
	}
	if len(data) > maxAnalysisTraceFileBytes {
		return ai.AnalysisTraceFile{}, fmt.Errorf("trace file exceeds %d bytes", maxAnalysisTraceFileBytes)
	}
	var traces ai.AnalysisTraceFile
	if err := json.Unmarshal(data, &traces); err != nil {
		return ai.AnalysisTraceFile{}, fmt.Errorf("decode trace file: %w", err)
	}
	if traces.Traces == nil {
		traces.Traces = []ai.AnalysisTrace{}
	}
	return traces, nil
}

func filterAnalysisTraces(traces []ai.AnalysisTrace, r *http.Request) []ai.AnalysisTrace {
	query := r.URL.Query()
	jobID := query.Get("job_id")
	buildID := query.Get("build_id")
	testName := query.Get("test_name")
	outcome := query.Get("outcome")
	responseID := query.Get("response_id")
	filtered := make([]ai.AnalysisTrace, 0, len(traces))
	for _, trace := range traces {
		if jobID != "" && trace.JobID != jobID || buildID != "" && trace.BuildID != buildID ||
			testName != "" && trace.TestName != testName || outcome != "" && trace.Outcome != outcome ||
			responseID != "" && !traceHasResponseID(trace, responseID) {
			continue
		}
		filtered = append(filtered, trace)
	}
	return filtered
}

func traceHasResponseID(trace ai.AnalysisTrace, responseID string) bool {
	for _, event := range trace.Events {
		if event.ResponseID == responseID {
			return true
		}
	}
	return false
}
