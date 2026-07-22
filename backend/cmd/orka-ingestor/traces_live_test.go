package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
)

func TestLiveOrkaTraceTranslation(t *testing.T) {
	if os.Getenv("ORKA_TRACE_LIVE") != "1" {
		t.Skip("set ORKA_TRACE_LIVE=1 with ORKA_API, ORKA_TOKEN, and ORKA_TASK")
	}
	endpoint := strings.TrimRight(os.Getenv("ORKA_API"), "/")
	token := os.Getenv("ORKA_TOKEN")
	taskName := os.Getenv("ORKA_TASK")
	namespace := os.Getenv("ORKA_NAMESPACE")
	if endpoint == "" || token == "" || taskName == "" {
		t.Fatal("ORKA_API, ORKA_TOKEN, and ORKA_TASK are required")
	}
	if namespace == "" {
		namespace = "orka-system"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := &orkaClient{base: endpoint, token: token, http: &http.Client{Timeout: 30 * time.Second}}
	telemetry, err := client.analysisTelemetry(ctx, namespace, taskName)
	if err != nil {
		t.Fatal(err)
	}
	trace := buildOrkaAnalysisTrace(orkaTraceIdentity{
		Namespace: namespace, TaskName: taskName, ContractHash: "live",
		JobID: "live", BuildID: "live", TestName: "live",
	}, telemetry)
	store := ai.NewTraceStore()
	store.Upsert(trace)
	path := filepath.Join(t.TempDir(), "ai_traces.json")
	if err := store.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot ai.AnalysisTraceFile
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Traces) != 1 || snapshot.Traces[0].TaskName != taskName || snapshot.Traces[0].APIMode == "" {
		t.Fatalf("live trace = %+v", snapshot.Traces)
	}
	foundResponse := false
	for _, event := range snapshot.Traces[0].Events {
		if event.ResponseID == telemetry.ResponseID && telemetry.ResponseID != "" {
			foundResponse = true
		}
	}
	if !foundResponse {
		t.Fatalf("live trace did not preserve response ID: %+v", snapshot.Traces[0])
	}
	for _, forbidden := range []string{"gemini-3.5-flash", `"provider"`, `"model"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("live trace persisted %q: %s", forbidden, raw)
		}
	}
}
