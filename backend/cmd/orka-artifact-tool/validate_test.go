package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/orka"
)

const testArtifactValidationKey = "test-validation-key"

type validationTreeBrowser struct {
	artifacts.Browser
	paths     []string
	truncated bool
}

func (b validationTreeBrowser) ListTree(context.Context, int) ([]string, bool, error) {
	return b.paths, b.truncated, nil
}

func TestValidateAnalysisRequiresReadEvidence(t *testing.T) {
	attestor := newEvidenceAttestor("secret")
	env := &toolEnv{evidence: attestor}
	analysis := orka.AnalysisValidation{
		Summary: "summary", RootCause: "cause", Severity: "High",
		SuggestedFix: "fix", RelevantFiles: []string{"build-log.txt"},
	}

	tests := []struct {
		name        string
		tokens      []string
		wantStatus  int
		wantValid   bool
		wantMissing string
	}{
		{name: "successfully read", tokens: []string{attestor.issue("scope", "build-log.txt")}, wantStatus: http.StatusOK, wantValid: true},
		{name: "not read", wantStatus: http.StatusUnprocessableEntity, wantMissing: "build-log.txt"},
		{name: "invalid token", tokens: []string{"invalid"}, wantStatus: http.StatusUnprocessableEntity},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := runValidation(t, env, analysis, tt.tokens, "scope", "")
			response := recorder.Result()
			defer response.Body.Close()
			if response.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.StatusCode, tt.wantStatus, recorder.Body.String())
			}
			var result struct {
				AllPresent      bool     `json:"all_present"`
				Missing         []string `json:"missing"`
				ValidationToken string   `json:"validation_token"`
			}
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if result.AllPresent != tt.wantValid {
				t.Errorf("all_present = %t, want %t", result.AllPresent, tt.wantValid)
			}
			if tt.wantValid && result.ValidationToken == "" {
				t.Error("successful validation did not return validation_token")
			}
			if tt.wantMissing != "" && (len(result.Missing) != 1 || result.Missing[0] != tt.wantMissing) {
				t.Errorf("missing = %v, want [%s]", result.Missing, tt.wantMissing)
			}
		})
	}
}

func TestValidateAnalysisRequiresRelevantFilesArray(t *testing.T) {
	env := &toolEnv{evidence: newEvidenceAttestor("secret")}
	req := httptest.NewRequest(http.MethodPost, "/tool/validate_analysis", strings.NewReader(`{"analysis":{"summary":"summary","root_cause":"cause","severity":"High","is_transient":false,"suggested_fix":"fix"},"evidence_tokens":[]}`))
	req.Header.Set(orka.ValidationKeyHeader, testArtifactValidationKey)
	req.Header.Set(orka.ValidationTaskHeader, "task")
	req.Header.Set(orka.MinGCSBytesHeader, "0")
	recorder := httptest.NewRecorder()
	validateAnalysis(env, recorder, req)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "relevant_files") {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestValidateAnalysisEnforcesMatchedSkillReadEvidence(t *testing.T) {
	set, err := skills.ParseContract([]byte(`{
		"skills":[{
			"id":"quota",
			"triggers":["(?i)quota"],
			"required_evidence":[{"id":"events","any_of":["events/.*quota"]}]
		}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	header, err := set.HeaderValue()
	if err != nil {
		t.Fatal(err)
	}
	attestor := newEvidenceAttestor("secret")
	env := &toolEnv{evidence: attestor}
	analysis := orka.AnalysisValidation{
		Summary: "summary", RootCause: "resource quota exceeded", Severity: "High",
		SuggestedFix: "increase quota", RelevantFiles: []string{"build-log.txt", "events/quota-event.log"},
	}

	missing := runValidation(t, env, analysis, []string{attestor.issue("scope", "build-log.txt")}, "scope", header)
	if missing.Code != http.StatusUnprocessableEntity || !strings.Contains(missing.Body.String(), "quota:events") {
		t.Fatalf("missing evidence response = %d %s", missing.Code, missing.Body.String())
	}
	valid := runValidation(t, env, analysis, []string{
		attestor.issue("scope", "build-log.txt"),
		attestor.issue("scope", "events/quota-event.log"),
	}, "scope", header)
	if valid.Code != http.StatusOK {
		t.Fatalf("valid evidence response = %d %s", valid.Code, valid.Body.String())
	}
}

func TestValidateAnalysisEnforcesMergedEngineEvidencePaths(t *testing.T) {
	set, _, err := skills.LoadForTools(t.TempDir(), []string{"filesystem", "k8s"})
	if err != nil {
		t.Fatal(err)
	}
	header, err := set.HeaderValue()
	if err != nil {
		t.Fatal(err)
	}
	attestor := newEvidenceAttestor("secret")
	env := &toolEnv{evidence: attestor}
	paths := map[string]string{
		"machine": "artifacts/clusters/bootstrap/resources/ns/Machine/machine.yaml",
		"node":    "artifacts/clusters/workload/nodes/node-1/node-describe.txt",
		"cloud":   "artifacts/clusters/workload/kube-system/cloud-node-manager-node-1/cloud-node-manager.log",
		"proxy":   "artifacts/clusters/workload/kube-system/kube-proxy-node-1/kube-proxy.log",
	}
	analysis := orka.AnalysisValidation{
		Summary:   "worker initialization blocked",
		RootCause: "the worker Node registered but providerID is missing and the external cloud-provider taint remains",
		Severity:  "High", SuggestedFix: "restore cloud-provider initialization",
		RelevantFiles: []string{paths["machine"]},
	}

	missing := runValidation(t, env, analysis, []string{
		attestor.issue("scope", paths["machine"]),
		attestor.issue("scope", paths["cloud"]),
		attestor.issue("scope", paths["proxy"]),
	}, "scope", header)
	if missing.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(missing.Body.String(), "engine.kubernetes.machine-node-providerid:node-state") {
		t.Fatalf("missing node evidence response = %d %s", missing.Code, missing.Body.String())
	}

	valid := runValidation(t, env, analysis, []string{
		attestor.issue("scope", paths["machine"]),
		attestor.issue("scope", paths["node"]),
		attestor.issue("scope", paths["cloud"]),
		attestor.issue("scope", paths["proxy"]),
	}, "scope", header)
	if valid.Code != http.StatusOK {
		t.Fatalf("valid merged evidence response = %d %s", valid.Code, valid.Body.String())
	}
}

func TestValidateAnalysisAppliesDNSWithoutServiceEvidence(t *testing.T) {
	set, _, err := skills.LoadForTools(t.TempDir(), []string{"filesystem", "k8s"})
	if err != nil {
		t.Fatal(err)
	}
	header, err := set.HeaderValue()
	if err != nil {
		t.Fatal(err)
	}
	attestor := newEvidenceAttestor("secret")
	env := &toolEnv{evidence: attestor}
	clientPath := "artifacts/clusters/workload/kube-system/kube-proxy-node-1/kube-proxy.log"
	resolverPath := "artifacts/clusters/workload/nodes/node-1/resolv.conf"
	analysis := orka.AnalysisValidation{
		Summary:   "resolver blocked API hostname lookup",
		RootCause: "the API hostname lookup used a loopback DNS resolver that refused connections",
		Severity:  "High", SuggestedFix: "restore the node resolver configuration",
		RelevantFiles: []string{clientPath},
	}

	missing := runValidation(t, env, analysis, []string{attestor.issue("scope", clientPath)}, "scope", header)
	if missing.Code != http.StatusUnprocessableEntity || !strings.Contains(missing.Body.String(), "engine.kubernetes.service-api-dns-connectivity:dns-resolution") {
		t.Fatalf("missing DNS evidence response = %d %s", missing.Code, missing.Body.String())
	}
	if strings.Contains(missing.Body.String(), "service-routing") {
		t.Fatalf("DNS-only draft required Service evidence: %s", missing.Body.String())
	}

	valid := runValidation(t, env, analysis, []string{
		attestor.issue("scope", clientPath),
		attestor.issue("scope", resolverPath),
	}, "scope", header)
	if valid.Code != http.StatusOK {
		t.Fatalf("valid DNS evidence response = %d %s", valid.Code, valid.Body.String())
	}
}

func TestValidateAnalysisPrunesRecipeEvidenceAbsentFromBuild(t *testing.T) {
	set, err := skills.ParseContract([]byte(`{
		"skills":[{
			"id":"quota",
			"triggers":["(?i)quota"],
			"required_evidence":[{"id":"events","any_of":["events/.*quota"]}]
		}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	header, err := set.HeaderValue()
	if err != nil {
		t.Fatal(err)
	}
	attestor := newEvidenceAttestor("secret")
	env := &toolEnv{
		evidence: attestor,
		browser:  validationTreeBrowser{paths: []string{"build-log.txt"}},
	}
	analysis := orka.AnalysisValidation{
		Summary: "summary", RootCause: "resource quota exceeded", Severity: "High",
		SuggestedFix: "increase quota", RelevantFiles: []string{"build-log.txt"},
	}
	response := runValidation(t, env, analysis, []string{attestor.issue("scope", "build-log.txt")}, "scope", header)
	if response.Code != http.StatusOK {
		t.Fatalf("absent recipe evidence response = %d %s", response.Code, response.Body.String())
	}
	env.browser = validationTreeBrowser{paths: []string{"build-log.txt"}, truncated: true}
	response = runValidation(t, env, analysis, []string{attestor.issue("scope", "build-log.txt")}, "scope", header)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("truncated tree response = %d %s", response.Code, response.Body.String())
	}
}

func runValidation(t *testing.T, env *toolEnv, analysis orka.AnalysisValidation, tokens []string, scope, skillHeader string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"analysis": analysis, "evidence_tokens": tokens})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/tool/validate_analysis", bytes.NewReader(body))
	req.Header.Set(orka.ToolScopeHeader, scope)
	req.Header.Set(orka.ValidationKeyHeader, testArtifactValidationKey)
	req.Header.Set(orka.ValidationTaskHeader, "task")
	req.Header.Set(orka.MinGCSBytesHeader, "0")
	if skillHeader != "" {
		req.Header.Set(skills.ContractHeader, skillHeader)
	}
	recorder := httptest.NewRecorder()
	validateAnalysis(env, recorder, req)
	return recorder
}

func TestValidateAnalysisChecksArtifactCitationsAcrossProse(t *testing.T) {
	attestor := newEvidenceAttestor("secret")
	env := &toolEnv{evidence: attestor}
	analysis := orka.AnalysisValidation{
		Summary: "summary", RootCause: "controller bug", Severity: "High",
		SuggestedFix: "update source", RelevantFiles: []string{"kustomize/cluster-template.yaml"},
	}
	response := runValidation(t, env, analysis, nil, "scope", "")
	if response.Code != http.StatusOK {
		t.Fatalf("source citation response = %d %s", response.Code, response.Body.String())
	}

	analysis.RootCause = "artifacts/manager.log shows the controller failure"
	response = runValidation(t, env, analysis, nil, "scope", "")
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "artifacts/manager.log") {
		t.Fatalf("unread prose citation response = %d %s", response.Code, response.Body.String())
	}
	response = runValidation(t, env, analysis, []string{attestor.issue("scope", "artifacts/manager.log")}, "scope", "")
	if response.Code != http.StatusOK {
		t.Fatalf("read prose citation response = %d %s", response.Code, response.Body.String())
	}
}

func TestValidateAnalysisEnforcesMinimumGCSBytes(t *testing.T) {
	attestor := newEvidenceAttestor("secret")
	env := &toolEnv{evidence: attestor}
	analysis := orka.AnalysisValidation{Summary: "summary", RootCause: "cause", Severity: "High", SuggestedFix: "fix", RelevantFiles: []string{"build-log.txt"}}
	body, err := json.Marshal(map[string]any{"analysis": analysis, "evidence_tokens": []string{attestor.issueBytes("scope", "build-log.txt", 10)}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/tool/validate_analysis", bytes.NewReader(body))
	req.Header.Set(orka.ToolScopeHeader, "scope")
	req.Header.Set(orka.ValidationKeyHeader, testArtifactValidationKey)
	req.Header.Set(orka.ValidationTaskHeader, "task")
	req.Header.Set(orka.MinGCSBytesHeader, "11")
	recorder := httptest.NewRecorder()
	validateAnalysis(env, recorder, req)
	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), `"gcs_bytes":10`) {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func runSubmission(t *testing.T, env *toolEnv, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/tool/submit_analysis", bytes.NewReader(data))
	req.Header.Set(orka.ToolScopeHeader, "scope")
	req.Header.Set(orka.ValidationKeyHeader, testArtifactValidationKey)
	req.Header.Set(orka.ValidationTaskHeader, "task")
	req.Header.Set(orka.MinGCSBytesHeader, "0")
	recorder := httptest.NewRecorder()
	submitAnalysis(env, recorder, req)
	return recorder
}

func TestSubmitAnalysisRejectsInvalidFinalShape(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"summary":         "summary",
			"root_cause":      "cause",
			"severity":        "High",
			"is_transient":    false,
			"suggested_fix":   "fix",
			"relevant_files":  []string{},
			"evidence_tokens": []string{},
		}
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{name: "summary", mutate: func(body map[string]any) { body["summary"] = " " }, want: "summary is required"},
		{name: "root cause", mutate: func(body map[string]any) { delete(body, "root_cause") }, want: "root_cause is required"},
		{name: "transient verdict", mutate: func(body map[string]any) { delete(body, "is_transient") }, want: "is_transient is required"},
		{name: "severity", mutate: func(body map[string]any) { body["severity"] = "severe" }, want: `severity "severe" is invalid`},
		{name: "suggested fix", mutate: func(body map[string]any) { body["suggested_fix"] = "" }, want: "suggested_fix is required"},
		{name: "relevant files", mutate: func(body map[string]any) { body["relevant_files"] = nil }, want: "relevant_files array is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := base()
			tc.mutate(body)
			response := runSubmission(t, &toolEnv{}, body)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), tc.want) {
				t.Fatalf("response = %d %s, want %q", response.Code, response.Body.String(), tc.want)
			}
		})
	}
}

func TestSubmitAnalysisAcceptsFlatSchema(t *testing.T) {
	attestor := newEvidenceAttestor("secret")
	env := &toolEnv{evidence: attestor}
	analysis := orka.AnalysisValidation{
		Summary: "summary", RootCause: "cause", Severity: "High",
		SuggestedFix: "fix", RelevantFiles: []string{"build-log.txt"},
	}
	token := attestor.issue("scope", "build-log.txt")
	recorder := runSubmission(t, env, map[string]any{
		"summary":         analysis.Summary,
		"root_cause":      analysis.RootCause,
		"severity":        analysis.Severity,
		"is_transient":    analysis.IsTransient,
		"suggested_fix":   analysis.SuggestedFix,
		"relevant_files":  analysis.RelevantFiles,
		"evidence_tokens": []string{token},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		GCSBytes        int    `json:"gcs_bytes"`
		ValidationToken string `json:"validation_token"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ValidationToken == "" || result.GCSBytes <= 0 {
		t.Fatalf("submission result = %+v", result)
	}
	if !orka.VerifyAnalysisValidationToken(
		testArtifactValidationKey,
		"task",
		analysis,
		result.GCSBytes,
		result.ValidationToken,
	) {
		t.Fatal("submission token did not bind the flat analysis")
	}
}
