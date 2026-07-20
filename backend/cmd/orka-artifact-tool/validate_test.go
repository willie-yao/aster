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
	req := httptest.NewRequest(http.MethodPost, "/tool/validate_analysis", strings.NewReader(`{"analysis":{"root_cause":"cause"},"evidence_tokens":[]}`))
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
