package analysisruntime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
)

const (
	// InlineRequestEnv carries one bounded FailureAnalysisRequest JSON value.
	InlineRequestEnv = "PROW_AI_FAILURE_REQUEST"
	// InlineRequestDigestEnv carries the canonical request SHA-256.
	InlineRequestDigestEnv = "PROW_AI_FAILURE_REQUEST_SHA256"
	// MaxInlineRequestBytes bounds the Kubernetes environment transport.
	MaxInlineRequestBytes = 64 << 10
)

// EncodeInlineRequest returns canonical bounded JSON and its SHA-256 digest.
func EncodeInlineRequest(request ai.FailureAnalysisRequest) ([]byte, string, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return nil, "", fmt.Errorf("marshal failure analysis request: %w", err)
	}
	if len(data) > MaxInlineRequestBytes {
		return nil, "", fmt.Errorf("failure analysis request is %d bytes, exceeds %d-byte inline limit", len(data), MaxInlineRequestBytes)
	}
	if err := validateRequest(request); err != nil {
		return nil, "", err
	}
	return data, digestBytes(data), nil
}

// DecodeInlineRequest parses one strict bounded request and returns its digest.
func DecodeInlineRequest(data []byte) (ai.FailureAnalysisRequest, string, error) {
	var request ai.FailureAnalysisRequest
	if len(data) == 0 {
		return request, "", fmt.Errorf("failure analysis request is empty")
	}
	if len(data) > MaxInlineRequestBytes {
		return request, "", fmt.Errorf("failure analysis request is %d bytes, exceeds %d-byte inline limit", len(data), MaxInlineRequestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, "", fmt.Errorf("decode failure analysis request: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return request, "", err
	}
	if err := validateRequest(request); err != nil {
		return request, "", err
	}
	canonical, err := json.Marshal(request)
	if err != nil {
		return request, "", fmt.Errorf("canonicalize failure analysis request: %w", err)
	}
	return request, digestBytes(canonical), nil
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing failure analysis request data: %w", err)
	}
	return fmt.Errorf("failure analysis request contains multiple JSON values")
}

func validateRequest(request ai.FailureAnalysisRequest) error {
	switch {
	case strings.TrimSpace(request.JobID) == "":
		return fmt.Errorf("failure analysis request job_id is required")
	case strings.TrimSpace(request.BuildPrefix) == "":
		return fmt.Errorf("failure analysis request build_prefix is required")
	case strings.TrimSpace(request.Build.BuildID) == "":
		return fmt.Errorf("failure analysis request build.build_id is required")
	case strings.TrimSpace(request.TestCase.Name) == "":
		return fmt.Errorf("failure analysis request test_case.name is required")
	case request.TestCase.Status != "failed":
		return fmt.Errorf("failure analysis request test_case.status must be failed")
	case request.ConsecutiveFailures < 0:
		return fmt.Errorf("failure analysis request consecutive_failures must not be negative")
	default:
		return nil
	}
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
