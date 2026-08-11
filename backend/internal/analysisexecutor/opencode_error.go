package analysisexecutor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/agentanalysis"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/redact"
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

type openCodeUnknownErrorData struct {
	Message *string         `json:"message"`
	Ref     *string         `json:"ref"`
	Cause   json.RawMessage `json:"cause"`
}

type openCodeUnknownCause struct {
	Name    *string         `json:"name"`
	Code    *string         `json:"code"`
	Message json.RawMessage `json:"message"`
	Cause   json.RawMessage `json:"cause"`
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
	if input.Name == "UnknownError" {
		if err := sanitizeOpenCodeUnknownError(input.Data, &result); err != nil {
			return agentanalysis.WorkspaceOpenCodeErrorTelemetry{}, err
		}
	} else if input.Name == "APIError" || input.Name == "ContextOverflowError" {
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

func sanitizeOpenCodeUnknownError(raw json.RawMessage, result *agentanalysis.WorkspaceOpenCodeErrorTelemetry) error {
	if len(raw) == 0 || len(raw) > maxOpenCodeErrorDataBytes {
		return fmt.Errorf("OpenCode unknown error data is empty or oversized")
	}
	var data openCodeUnknownErrorData
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&data); err != nil {
		return fmt.Errorf("decode OpenCode unknown error data")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("OpenCode unknown error data contains trailing values")
	}
	if data.Message == nil {
		return fmt.Errorf("OpenCode unknown error message is unavailable")
	}
	if data.Ref != nil && len(*data.Ref) > maxOpenCodeErrorFieldBytes {
		return fmt.Errorf("OpenCode unknown error reference is oversized")
	}
	message := *data.Message
	result.MessagePresent = true
	result.MessageBytes = len([]byte(message))
	redacted := redact.Credentials(redact.URLs(message))
	digest := sha256.Sum256([]byte(redacted))
	result.RedactedMessageSHA256 = hex.EncodeToString(digest[:])
	result.Classification = classifyOpenCodeUnknownMessage(message)
	result.CauseName, result.CauseCode = sanitizeOpenCodeUnknownCause(data.Cause)
	if result.CauseName == "" {
		result.CauseName = allowlistedOpenCodeCauseNameInMessage(message)
	}
	if result.CauseCode == "" {
		result.CauseCode = allowlistedOpenCodeCauseCodeInMessage(message)
	}
	if result.Classification == "unknown" {
		result.Classification = classifyOpenCodeUnknownCause(result.CauseName, result.CauseCode)
	}
	return nil
}

func sanitizeOpenCodeUnknownCause(raw json.RawMessage) (string, string) {
	var name, code string
	for depth := 0; depth < 4 && len(raw) > 0 && string(raw) != "null"; depth++ {
		if len(raw) > maxOpenCodeErrorFieldBytes {
			break
		}
		var cause openCodeUnknownCause
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cause); err != nil {
			break
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			break
		}
		if cause.Name != nil {
			if candidate := allowlistedOpenCodeCauseName(*cause.Name); candidate != "" {
				name = candidate
			}
		}
		if cause.Code != nil {
			if candidate := allowlistedOpenCodeCauseCode(*cause.Code); candidate != "" {
				code = candidate
			}
		}
		raw = cause.Cause
	}
	return name, code
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
		if value.Classification != "" {
			return value.Classification
		}
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

func classifyOpenCodeUnknownMessage(value string) string {
	lower := strings.ToLower(value)
	switch {
	case containsAny(lower, "certificate", "self signed", "self-signed", "tls", "ssl", "unable_to_verify_leaf_signature"):
		return "tls"
	case containsAny(lower, "getaddrinfo", "enotfound", "eai_again", "dns"):
		return "dns"
	case containsAny(lower, "econnreset", "connection reset", "socket closed"):
		return "connection_reset"
	case containsAny(lower, "econnrefused", "connection refused"):
		return "connection_refused"
	case containsAny(lower, "providerheadertimeouterror", "headers timeout", "header timeout", "und_err_headers_timeout"):
		return "header_timeout"
	case containsAny(lower, "providerresponsestreamerror", "response stream", "stream processing", "stream error"):
		return "response_stream"
	case containsAny(lower, "invalid tool schema", "tool schema is invalid", "invalid function schema"):
		return "invalid_tool_schema"
	case containsAny(lower, "permission denied", "permission rejected", "eacces", "eperm", "not permitted"):
		return "permission_denied"
	case containsAny(lower, "sqlite", "database", "sql query", "sql statement"):
		return "database"
	case containsAny(lower, "read-only file system", "readonly filesystem", "filesystem", "no such file", "enoent", "erofs", "enospc"):
		return "filesystem"
	case containsAny(lower, "serialize", "serialization", "deserialize", "json parse", "unexpected token in json"):
		return "serialization"
	case containsAny(lower, "apicallerror", "provider api", "provider error", "api request"):
		return "provider_api"
	default:
		return "unknown"
	}
}

func classifyOpenCodeUnknownCause(name, code string) string {
	switch code {
	case "CERT_HAS_EXPIRED", "DEPTH_ZERO_SELF_SIGNED_CERT", "SELF_SIGNED_CERT_IN_CHAIN", "UNABLE_TO_VERIFY_LEAF_SIGNATURE":
		return "tls"
	case "ENOTFOUND", "EAI_AGAIN":
		return "dns"
	case "ECONNRESET":
		return "connection_reset"
	case "ECONNREFUSED":
		return "connection_refused"
	case "UND_ERR_HEADERS_TIMEOUT":
		return "header_timeout"
	case "EACCES", "EPERM":
		return "permission_denied"
	case "EROFS", "ENOENT", "ENOSPC":
		return "filesystem"
	case "SQLITE_READONLY", "SQLITE_CANTOPEN", "SQLITE_IOERR", "SQLITE_BUSY", "SQLITE_FULL":
		return "database"
	case "ERR_INVALID_ARG_TYPE":
		return "serialization"
	}
	switch name {
	case "ProviderResponseStreamError":
		return "response_stream"
	case "HeadersTimeoutError", "ConnectTimeoutError":
		return "header_timeout"
	case "PermissionError":
		return "permission_denied"
	case "FilesystemError":
		return "filesystem"
	case "DatabaseError", "SqliteError":
		return "database"
	case "SerializationError":
		return "serialization"
	case "APICallError":
		return "provider_api"
	default:
		return "unknown"
	}
}

func allowlistedOpenCodeCauseName(value string) string {
	switch strings.TrimSpace(value) {
	case "Error", "TypeError", "DOMException", "SystemError", "FetchError", "ConnectTimeoutError", "HeadersTimeoutError", "SocketError", "ProviderResponseStreamError", "PermissionError", "FilesystemError", "DatabaseError", "SqliteError", "SerializationError", "APICallError":
		return strings.TrimSpace(value)
	default:
		return ""
	}
}

func allowlistedOpenCodeCauseNameInMessage(value string) string {
	lower := strings.ToLower(value)
	for _, candidate := range []string{"ProviderResponseStreamError", "ConnectTimeoutError", "HeadersTimeoutError", "SerializationError", "FilesystemError", "PermissionError", "DatabaseError", "APICallError", "DOMException", "SystemError", "FetchError", "SocketError", "SqliteError", "TypeError"} {
		if strings.Contains(lower, strings.ToLower(candidate)) {
			return candidate
		}
	}
	if strings.HasPrefix(strings.TrimSpace(lower), "error:") {
		return "Error"
	}
	return ""
}

func allowlistedOpenCodeCauseCode(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	switch value {
	case "ECONNRESET", "ECONNREFUSED", "ENOTFOUND", "EAI_AGAIN", "ETIMEDOUT", "UND_ERR_HEADERS_TIMEOUT", "CERT_HAS_EXPIRED", "DEPTH_ZERO_SELF_SIGNED_CERT", "SELF_SIGNED_CERT_IN_CHAIN", "UNABLE_TO_VERIFY_LEAF_SIGNATURE", "EACCES", "EPERM", "EROFS", "ENOENT", "ENOSPC", "SQLITE_READONLY", "SQLITE_CANTOPEN", "SQLITE_IOERR", "SQLITE_BUSY", "SQLITE_FULL", "ERR_INVALID_ARG_TYPE":
		return value
	default:
		return ""
	}
}

func allowlistedOpenCodeCauseCodeInMessage(value string) string {
	upper := strings.ToUpper(value)
	for _, candidate := range []string{"UND_ERR_HEADERS_TIMEOUT", "DEPTH_ZERO_SELF_SIGNED_CERT", "SELF_SIGNED_CERT_IN_CHAIN", "UNABLE_TO_VERIFY_LEAF_SIGNATURE", "ERR_INVALID_ARG_TYPE", "SQLITE_READONLY", "SQLITE_CANTOPEN", "SQLITE_IOERR", "SQLITE_BUSY", "SQLITE_FULL", "CERT_HAS_EXPIRED", "ECONNREFUSED", "ECONNRESET", "ENOTFOUND", "EAI_AGAIN", "ETIMEDOUT", "EACCES", "EPERM", "EROFS", "ENOENT", "ENOSPC"} {
		if strings.Contains(upper, candidate) {
			return candidate
		}
	}
	return ""
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func allowlistedOpenCodeMetadataCode(value string) string {
	switch strings.TrimSpace(value) {
	case "ProviderHeaderTimeoutError", "ProviderResponseStreamError", "ECONNRESET", "ZlibError":
		return strings.TrimSpace(value)
	default:
		return ""
	}
}
