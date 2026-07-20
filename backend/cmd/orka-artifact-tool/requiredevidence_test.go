package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai/skills"
)

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
