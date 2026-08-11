package analysisexecutor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
)

const (
	maxOpenCodeErrorDataBytes  = 1 << 20
	maxOpenCodeErrorFieldBytes = 8 << 10
	maxOpenCodeErrorMapEntries = 64
)

type openCodeErrorEnvelope struct {
	Name string          `json:"name"`
	Data json.RawMessage `json:"data"`
}

type openCodeErrorData struct {
	Message         *string           `json:"message"`
	StatusCode      *int              `json:"statusCode"`
	IsRetryable     *bool             `json:"isRetryable"`
	ResponseHeaders map[string]string `json:"responseHeaders"`
	ResponseBody    *string           `json:"responseBody"`
	Metadata        map[string]string `json:"metadata"`
}

type openCodePromptError struct {
	name      string
	telemetry agentanalysis.WorkspaceOpenCodeErrorTelemetry
}

func (e *openCodePromptError) Error() string {
	return "OpenCode structured output failed: " + e.name
}

func sanitizeOpenCodeError(input *openCodeErrorEnvelope) (agentanalysis.WorkspaceOpenCodeErrorTelemetry, error) {
	if input == nil {
		return agentanalysis.WorkspaceOpenCodeErrorTelemetry{}, nil
	}
	if !recognizedOpenCodeError(input.Name) || len(input.Name) > maxOpenCodeFieldBytes {
		return agentanalysis.WorkspaceOpenCodeErrorTelemetry{}, fmt.Errorf("OpenCode error name is invalid")
	}
	result := agentanalysis.WorkspaceOpenCodeErrorTelemetry{Available: true, Name: input.Name}
	if input.Name == "APIError" || input.Name == "ContextOverflowError" {
		if len(input.Data) == 0 || len(input.Data) > maxOpenCodeErrorDataBytes {
			return agentanalysis.WorkspaceOpenCodeErrorTelemetry{}, fmt.Errorf("OpenCode error data is empty or oversized")
		}
		var data openCodeErrorData
		decoder := json.NewDecoder(strings.NewReader(string(input.Data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&data); err != nil {
			return agentanalysis.WorkspaceOpenCodeErrorTelemetry{}, fmt.Errorf("decode OpenCode error data")
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return agentanalysis.WorkspaceOpenCodeErrorTelemetry{}, fmt.Errorf("OpenCode error data contains trailing values")
		}
		if data.Message == nil || len(*data.Message) > maxOpenCodeErrorFieldBytes {
			return agentanalysis.WorkspaceOpenCodeErrorTelemetry{}, fmt.Errorf("OpenCode error message is invalid")
		}
		if data.StatusCode != nil {
			if *data.StatusCode < 100 || *data.StatusCode > 599 {
				return agentanalysis.WorkspaceOpenCodeErrorTelemetry{}, fmt.Errorf("OpenCode error status is invalid")
			}
			result.HTTPStatusCode = *data.StatusCode
		}
		if input.Name == "APIError" && data.IsRetryable == nil {
			return agentanalysis.WorkspaceOpenCodeErrorTelemetry{}, fmt.Errorf("OpenCode API error retryability is unavailable")
		}
		if data.IsRetryable != nil {
			result.RetryableKnown = true
			result.Retryable = *data.IsRetryable
		}
		if len(data.ResponseHeaders) > maxOpenCodeErrorMapEntries || len(data.Metadata) > maxOpenCodeErrorMapEntries {
			return agentanalysis.WorkspaceOpenCodeErrorTelemetry{}, fmt.Errorf("OpenCode error map is oversized")
		}
		for name, value := range data.ResponseHeaders {
			if len(name) > maxOpenCodeErrorFieldBytes || len(value) > maxOpenCodeErrorFieldBytes {
				return agentanalysis.WorkspaceOpenCodeErrorTelemetry{}, fmt.Errorf("OpenCode error header is oversized")
			}
			if strings.EqualFold(strings.TrimSpace(name), "content-type") && strings.TrimSpace(value) != "" {
				result.ResponseContentTypePresent = true
			}
		}
		for name, value := range data.Metadata {
			if len(name) > maxOpenCodeErrorFieldBytes || len(value) > maxOpenCodeErrorFieldBytes {
				return agentanalysis.WorkspaceOpenCodeErrorTelemetry{}, fmt.Errorf("OpenCode error metadata is oversized")
			}
		}
		result.MetadataCode = allowlistedOpenCodeMetadataCode(data.Metadata["code"])
		if data.ResponseBody != nil && *data.ResponseBody != "" {
			body := []byte(*data.ResponseBody)
			result.ResponseBodyPresent = true
			result.ResponseBodyBytesBounded = min(len(body), maxOpenCodeErrorDataBytes)
			digest := sha256.Sum256(body)
			result.ResponseBodySHA256 = hex.EncodeToString(digest[:])
		}
	}
	result.Classification = classifyOpenCodeError(result)
	result.HeaderTimeout = result.Classification == "header_timeout"
	result.ResponseStreamError = result.Classification == "response_stream"
	result.ContextOverflow = result.Classification == "context_overflow"
	return result, nil
}

func classifyOpenCodeError(value agentanalysis.WorkspaceOpenCodeErrorTelemetry) string {
	switch value.Name {
	case "ContextOverflowError":
		return "context_overflow"
	case "StructuredOutputError":
		return "structured_output"
	case "ProviderAuthError":
		return "provider_auth"
	case "MessageOutputLengthError":
		return "output_length"
	case "MessageAbortedError":
		return "aborted"
	case "ContentFilterError":
		return "content_filter"
	case "UnknownError":
		return "unknown"
	case "APIError":
		switch value.MetadataCode {
		case "ProviderHeaderTimeoutError":
			return "header_timeout"
		case "ProviderResponseStreamError":
			return "response_stream"
		}
		switch value.HTTPStatusCode {
		case 400:
			return "api_bad_request"
		case 401:
			return "api_unauthorized"
		case 403:
			return "api_forbidden"
		case 408:
			return "api_timeout"
		case 413:
			return "api_request_too_large"
		case 429:
			return "api_rate_limited"
		}
		if value.HTTPStatusCode >= 500 {
			return "api_server_error"
		}
		return "api_error"
	default:
		return "unknown"
	}
}

func allowlistedOpenCodeMetadataCode(value string) string {
	switch strings.TrimSpace(value) {
	case "ProviderHeaderTimeoutError", "ProviderResponseStreamError", "ECONNRESET", "ZlibError":
		return strings.TrimSpace(value)
	default:
		return ""
	}
}
