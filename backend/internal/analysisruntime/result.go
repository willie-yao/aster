package analysisruntime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/ai"
)

const (
	// FailureAnalysisResultMarker prefixes the analyzer's framed result line.
	FailureAnalysisResultMarker = "PROW_AI_RESULT_B64:"
	// MaxFailureAnalysisResultBytes bounds the decoded result payload.
	MaxFailureAnalysisResultBytes = 2 << 20
)

// WriteFailureAnalysisResult writes one bounded base64 result marker.
func WriteFailureAnalysisResult(w io.Writer, result ai.FailureAnalysisResult) error {
	if err := validateResult(result); err != nil {
		return err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode failure analysis result: %w", err)
	}
	if len(data) > MaxFailureAnalysisResultBytes {
		return fmt.Errorf("failure analysis result is %d bytes, exceeds %d-byte limit", len(data), MaxFailureAnalysisResultBytes)
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	if _, err := fmt.Fprintf(w, "%s%s\n", FailureAnalysisResultMarker, encoded); err != nil {
		return fmt.Errorf("write failure analysis result: %w", err)
	}
	return nil
}

// ParseFailureAnalysisResult extracts the last valid framed result from mixed logs.
func ParseFailureAnalysisResult(raw string) (ai.FailureAnalysisResult, error) {
	var (
		lastResult  ai.FailureAnalysisResult
		lastPayload []byte
		lastErr     error
		markerSeen  bool
	)
	for len(raw) > 0 {
		line, rest, found := strings.Cut(raw, "\n")
		if found {
			raw = rest
		} else {
			raw = ""
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, FailureAnalysisResultMarker) {
			continue
		}
		markerSeen = true
		result, payload, err := decodeFailureAnalysisResult(strings.TrimSpace(strings.TrimPrefix(line, FailureAnalysisResultMarker)))
		if err != nil {
			lastErr = err
			continue
		}
		if lastPayload != nil && !bytes.Equal(lastPayload, payload) {
			return ai.FailureAnalysisResult{}, fmt.Errorf("failure analysis logs contain conflicting result markers")
		}
		lastPayload = payload
		lastResult = result
	}
	if lastPayload != nil {
		return lastResult, nil
	}
	if markerSeen {
		return ai.FailureAnalysisResult{}, fmt.Errorf("failure analysis logs contain no valid result marker: %w", lastErr)
	}
	return ai.FailureAnalysisResult{}, fmt.Errorf("failure analysis result marker is missing")
}

func decodeFailureAnalysisResult(encoded string) (ai.FailureAnalysisResult, []byte, error) {
	var result ai.FailureAnalysisResult
	if encoded == "" {
		return result, nil, fmt.Errorf("failure analysis result marker is empty")
	}
	if len(encoded) > base64.StdEncoding.EncodedLen(MaxFailureAnalysisResultBytes) {
		return result, nil, fmt.Errorf("failure analysis result exceeds %d bytes", MaxFailureAnalysisResultBytes)
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return result, nil, fmt.Errorf("decode failure analysis result base64: %w", err)
	}
	if len(data) > MaxFailureAnalysisResultBytes {
		return result, nil, fmt.Errorf("failure analysis result exceeds %d bytes", MaxFailureAnalysisResultBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, nil, fmt.Errorf("decode failure analysis result JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return result, nil, fmt.Errorf("failure analysis result contains multiple JSON values")
	} else if err != io.EOF {
		return result, nil, fmt.Errorf("decode trailing failure analysis result data: %w", err)
	}
	if err := validateResult(result); err != nil {
		return result, nil, err
	}
	return result, data, nil
}

func validateResult(result ai.FailureAnalysisResult) error {
	if result.Summary == nil {
		return fmt.Errorf("failure analysis result has no ai_summary")
	}
	if strings.TrimSpace(result.Summary.Summary) == "" {
		return fmt.Errorf("failure analysis result has an empty ai_summary.summary")
	}
	return nil
}
