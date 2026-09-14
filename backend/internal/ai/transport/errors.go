package transport

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// HTTPError retains only bounded provider metadata safe to surface.
type HTTPError struct {
	API        string
	StatusCode int
	Category   string
	retryAfter string
	requestID  string
}

func (e *HTTPError) Error() string {
	message := fmt.Sprintf("%s returned %d", e.API, e.StatusCode)
	if e.Category == "tools_unsupported" {
		message += ": function calling unsupported"
	}
	if e.requestID != "" {
		message += " request_id=" + e.requestID
	}
	if e.retryAfter != "" {
		message += " retry_after=" + e.retryAfter
	}
	return message
}

// NewHTTPError classifies a provider failure without retaining its response body.
func NewHTTPError(api string, statusCode int, body string, header http.Header) *HTTPError {
	category := "http_error"
	if (statusCode == http.StatusBadRequest || statusCode == http.StatusUnprocessableEntity) && toolsUnsupportedRe.MatchString(body) {
		category = "tools_unsupported"
	}
	return &HTTPError{
		API: api, StatusCode: statusCode, Category: category,
		retryAfter: safeProviderRetryAfter(header.Get("Retry-After")), requestID: safeProviderRequestID(providerRequestID(header)),
	}
}

// RetryAfter returns the bounded retry header, if present.
func (e *HTTPError) RetryAfter() string { return safeProviderRetryAfter(e.retryAfter) }

// RequestID returns the bounded provider request identifier, if present.
func (e *HTTPError) RequestID() string { return safeProviderRequestID(e.requestID) }

func providerRequestID(header http.Header) string {
	for _, name := range []string{"X-GitHub-Request-Id", "OpenAI-Request-Id", "X-Request-Id", "Request-Id"} {
		if value := header.Get(name); value != "" {
			return value
		}
	}
	return ""
}

func safeProviderRetryAfter(value string) string {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 && seconds <= 86_400 {
		return strconv.Itoa(seconds)
	}
	if at, err := http.ParseTime(value); err == nil {
		return at.UTC().Format(http.TimeFormat)
	}
	return ""
}

func safeProviderRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:/", r) {
			continue
		}
		return ""
	}
	return value
}

var toolsUnsupportedRe = regexp.MustCompile(`(?i)tool[s_]?call|function[s_]?call|tools_choice|tools provided|tools?\s+(?:are\s+)?not supported|function calling`)

// IsToolsUnsupportedError recognizes tool rejection in bounded HTTP errors.
func IsToolsUnsupportedError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, " 400") && !strings.Contains(msg, " 422") {
		return false
	}
	return toolsUnsupportedRe.MatchString(msg)
}
