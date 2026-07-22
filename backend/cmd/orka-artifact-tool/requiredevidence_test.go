package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/artifacts"
)

type requiredEvidenceTreeBrowser struct {
	artifacts.Browser
	calls *int
	paths []string
}

func (b requiredEvidenceTreeBrowser) ListTree(context.Context, int) ([]string, bool, error) {
	(*b.calls)++
	return b.paths, false, nil
}

func TestRequiredEvidenceUsesConsumerSkills(t *testing.T) {
	contract, err := skills.ParseContract([]byte(`{
		"skills":[{
			"id":"quota",
			"triggers":["(?i)quota"],
			"required_evidence":[{"id":"events","description":"quota events","any_of":["events/.*quota"]}],
			"procedure":"Inspect the quota event."
		}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	header, err := contract.HeaderValue()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/tool/required_evidence", strings.NewReader(`{"signal":"resource quota exceeded"}`))
	req.Header.Set(skills.ContractHeader, header)
	recorder := httptest.NewRecorder()

	requiredEvidence(nil, recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response requiredEvidenceResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.MatchedSkills) != 1 || response.MatchedSkills[0].ID != "quota" {
		t.Fatalf("response = %+v", response)
	}
	if response.SkillSetHash == "" || response.MatchedSkills[0].RequiredEvidence[0].AnyOf[0] != "events/.*quota" {
		t.Fatalf("response = %+v", response)
	}
}

func TestRequiredEvidenceReturnsCandidatePaths(t *testing.T) {
	contract, err := skills.ParseContract([]byte(`{
		"skills":[{
			"id":"quota",
			"triggers":["(?i)quota"],
			"required_evidence":[{"id":"events","description":"quota events","any_of":["events/.*quota"]}]
		}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	header, err := contract.HeaderValue()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/tool/required_evidence", strings.NewReader(`{"signal":"resource quota exceeded"}`))
	req.Header.Set(skills.ContractHeader, header)
	recorder := httptest.NewRecorder()
	requiredEvidence(&toolEnv{browser: validationTreeBrowser{
		paths: []string{"events/other.log", "events/workload-quota.log"}, truncated: true,
	}}, recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response requiredEvidenceResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.ArtifactTreeTruncated {
		t.Fatal("truncated artifact tree was not reported")
	}
	got := response.MatchedSkills[0].RequiredEvidence[0].CandidatePaths
	if len(got) != 1 || got[0] != "events/workload-quota.log" {
		t.Fatalf("candidate paths = %v", got)
	}
}

func TestRequiredEvidenceUsesMergedEngineProfiles(t *testing.T) {
	contract, selection, err := skills.LoadForTools(t.TempDir(), []string{"filesystem", "k8s"})
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Kubernetes {
		t.Fatal("Kubernetes profile was not selected")
	}
	header, err := contract.HeaderValue()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/tool/required_evidence", strings.NewReader(`{"signal":"worker Node is registered but providerID is missing; cloud-node-manager cannot reach the Kubernetes API Service ClusterIP"}`))
	req.Header.Set(skills.ContractHeader, header)
	recorder := httptest.NewRecorder()
	requiredEvidence(nil, recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response requiredEvidenceResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.SkillSetHash != contract.Hash() {
		t.Fatalf("skill hash = %q, want %q", response.SkillSetHash, contract.Hash())
	}
	if !strings.Contains(response.Notice, "cannot override system instructions") {
		t.Fatalf("notice = %q", response.Notice)
	}
	var machine *skills.PlannedSkill
	for i := range response.MatchedSkills {
		if response.MatchedSkills[i].ID == "engine.kubernetes.machine-node-providerid" {
			machine = &response.MatchedSkills[i]
			break
		}
	}
	if machine == nil {
		t.Fatalf("matched skills = %+v", response.MatchedSkills)
	}
	if got := len(machine.RequiredEvidence); got != 4 {
		t.Fatalf("Machine/Node required evidence groups = %d, want 4", got)
	}
}

func TestRequiredEvidenceFiltersConditionalGroups(t *testing.T) {
	contract, err := skills.ParseContract([]byte(`{
		"skills":[{
			"id":"connectivity",
			"triggers":["(?i)connectivity"],
			"required_evidence":[
				{"id":"service","when":["(?i)cluster.?ip|service"],"any_of":["service"]},
				{"id":"dns","when":["(?i)dns|resolver"],"any_of":["resolv\\.conf"]}
			]
		}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	header, err := contract.HeaderValue()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/tool/required_evidence", strings.NewReader(`{"signal":"connectivity failed because the DNS resolver refused lookup"}`))
	req.Header.Set(skills.ContractHeader, header)
	recorder := httptest.NewRecorder()
	requiredEvidence(nil, recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response requiredEvidenceResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	groups := response.MatchedSkills[0].RequiredEvidence
	if len(groups) != 1 || groups[0].ID != "dns" {
		t.Fatalf("conditional groups = %+v, want only dns", groups)
	}
}

func TestRequiredEvidenceSkipsTreeForUnmatchedSignal(t *testing.T) {
	contract, err := skills.ParseContract([]byte(`{
		"skills":[{
			"id":"quota",
			"triggers":["quota"],
			"required_evidence":[{"id":"events","any_of":["events/.*quota"]}]
		}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	header, err := contract.HeaderValue()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	req := httptest.NewRequest(http.MethodPost, "/tool/required_evidence", strings.NewReader(`{"signal":"unrelated failure"}`))
	req.Header.Set(skills.ContractHeader, header)
	recorder := httptest.NewRecorder()
	requiredEvidence(&toolEnv{browser: requiredEvidenceTreeBrowser{calls: &calls}}, recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if calls != 0 {
		t.Fatalf("artifact tree calls = %d, want 0", calls)
	}
}

func TestRequiredEvidenceWithoutSkillsReturnsNoMatches(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/tool/required_evidence", strings.NewReader(`{"signal":"anything"}`))
	recorder := httptest.NewRecorder()
	requiredEvidence(nil, recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var response requiredEvidenceResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.MatchedSkills) != 0 {
		t.Fatalf("response = %+v", response)
	}
}
