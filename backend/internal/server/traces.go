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
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/output"
)

const maxAnalysisTraceFileBytes = 64 << 20

func analysisTracesHandler(dataDir string, attachment bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
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
	for i := range traces.Traces {
		if traces.Traces[i].Backend == "" {
			traces.Traces[i].Backend = "inprocess"
		}
	}
	return traces, nil
}

func filterAnalysisTraces(traces []ai.AnalysisTrace, r *http.Request) []ai.AnalysisTrace {
	query := r.URL.Query()
	jobID := query.Get("job_id")
	buildID := query.Get("build_id")
	testName := query.Get("test_name")
	backend := query.Get("backend")
	taskNamespace := query.Get("task_namespace")
	taskName := query.Get("task_name")
	contractHash := query.Get("contract_hash")
	outcome := query.Get("outcome")
	responseID := query.Get("response_id")
	filtered := make([]ai.AnalysisTrace, 0, len(traces))
	for _, trace := range traces {
		if jobID != "" && trace.JobID != jobID || buildID != "" && trace.BuildID != buildID ||
			testName != "" && trace.TestName != testName || backend != "" && trace.Backend != backend ||
			taskNamespace != "" && trace.TaskNamespace != taskNamespace || taskName != "" && trace.TaskName != taskName ||
			contractHash != "" && trace.ContractHash != contractHash ||
			outcome != "" && trace.Outcome != outcome || responseID != "" && !traceHasResponseID(trace, responseID) {
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
