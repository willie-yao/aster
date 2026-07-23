package analysisruntime

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/models"
)

func testRequest() ai.FailureAnalysisRequest {
	return ai.FailureAnalysisRequest{
		JobID:       "periodic-job",
		BuildPrefix: "logs/periodic-job/1/",
		Build: models.BuildInfo{
			JobName: "periodic-job", BuildID: "1",
			JUnitURLs: []string{"artifacts/junit.xml"}, RepoRefs: map[string]string{"repo": "sha"},
		},
		TestCase:            models.TestCase{Name: "Test A", Status: "failed", FailureMessage: "boom"},
		ConsecutiveFailures: 3,
	}
}

func TestInlineRequestRoundTripAndDigestStability(t *testing.T) {
	request := testRequest()
	before := testRequest()
	data, digest, err := EncodeInlineRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, data, "", "  "); err != nil {
		t.Fatal(err)
	}
	got, formattedDigest, err := DecodeInlineRequest(formatted.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if digest != formattedDigest {
		t.Fatalf("digests differ for equivalent JSON: %s and %s", digest, formattedDigest)
	}
	if !reflect.DeepEqual(got, request) {
		t.Fatalf("request = %+v, want %+v", got, request)
	}
	if !reflect.DeepEqual(request, before) {
		t.Fatalf("EncodeInlineRequest mutated request: %+v", request)
	}
}

func TestInlineRequestRejectsOversizedInput(t *testing.T) {
	request := testRequest()
	request.TestCase.FailureBody = strings.Repeat("x", MaxInlineRequestBytes)
	if _, _, err := EncodeInlineRequest(request); err == nil || !strings.Contains(err.Error(), "inline limit") {
		t.Fatalf("EncodeInlineRequest error = %v", err)
	}
	if _, _, err := DecodeInlineRequest(bytes.Repeat([]byte("x"), MaxInlineRequestBytes+1)); err == nil || !strings.Contains(err.Error(), "inline limit") {
		t.Fatalf("DecodeInlineRequest error = %v", err)
	}
}

func TestInlineRequestRejectsMalformedOrIncompleteInput(t *testing.T) {
	for _, raw := range []string{
		`not json`,
		`{"job_id":"job"} {}`,
		`{"job_id":"job","unknown":true}`,
		`{"job_id":"job","build_prefix":"logs/job/1/","build":{"build_id":"1"},"test_case":{"name":"Test A","status":"passed"}}`,
	} {
		if _, _, err := DecodeInlineRequest([]byte(raw)); err == nil {
			t.Fatalf("DecodeInlineRequest(%q) succeeded", raw)
		}
	}
}
