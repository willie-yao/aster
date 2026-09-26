package executor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/willie-yao/aster/backend/internal/redact"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
	"github.com/willie-yao/aster/backend/internal/textutil"
)

const (
	maxOpenCodeFailureDetailBytes = 300
	maxProviderErrorCodeBytes     = 64
)

type openCodeErrorEvent struct {
	Type  string `json:"type"`
	Error struct {
		Name string `json:"name"`
		Data struct {
			StatusCode   *int   `json:"statusCode"`
			Message      string `json:"message"`
			ProviderID   string `json:"providerID"`
			ResponseBody string `json:"responseBody"`
		} `json:"data"`
	} `json:"error"`
}

func parseOpenCodeErrorEvent(line string) (openCodeErrorEvent, bool) {
	var event openCodeErrorEvent
	if json.Unmarshal([]byte(strings.TrimSpace(line)), &event) != nil || event.Type != "error" {
		return openCodeErrorEvent{}, false
	}
	return event, true
}

func providerRejection(stdout string) (engineruntime.ExecutionFailureCode, string, *engineruntime.ProviderErrorDetail, bool) {
	for line := range strings.SplitSeq(stdout, "\n") {
		event, ok := parseOpenCodeErrorEvent(line)
		if !ok {
			continue
		}
		status := event.Error.Data.StatusCode
		switch {
		case event.Error.Name == "ProviderAuthError",
			event.Error.Name == "APIError" && status != nil && (*status == 401 || *status == 403):
			detail := providerErrorDetail(event)
			return engineruntime.ExecutionFailureProviderCredential, providerRejectionReason(detail.StatusCode), detail, true
		case event.Error.Name == "APIError" && status != nil && *status >= 400 && *status < 500 && *status != 408 && *status != 429:
			detail := providerErrorDetail(event)
			reason := fmt.Sprintf("model provider rejected the request (HTTP %d", detail.StatusCode)
			if detail.Code != "" {
				reason += " " + detail.Code
			}
			return engineruntime.ExecutionFailureProviderRequest, reason + ")", detail, true
		}
	}
	return "", "", nil, false
}

func openCodeFailureDetail(stdout, stderr string) string {
	var last openCodeErrorEvent
	found := false
	for line := range strings.SplitSeq(stdout, "\n") {
		if event, ok := parseOpenCodeErrorEvent(line); ok {
			last, found = event, true
		}
	}
	var detail string
	if found {
		detail = last.Error.Name
		if last.Error.Data.StatusCode != nil {
			detail += fmt.Sprintf(" HTTP %d", *last.Error.Data.StatusCode)
		}
		if code := providerErrorCode(last.Error.Data.ResponseBody); code != "" {
			detail += " " + code
		}
		if message := strings.TrimSpace(last.Error.Data.Message); message != "" {
			if detail != "" {
				detail += ": "
			}
			detail += message
		}
	}
	if detail == "" {
		for line := range strings.SplitSeq(stderr, "\n") {
			if strings.TrimSpace(line) != "" {
				detail = line
			}
		}
	}
	detail = redact.OperatorText(detail)
	if len(detail) > maxOpenCodeFailureDetailBytes {
		return textutil.Truncate(detail, maxOpenCodeFailureDetailBytes-len("…"))
	}
	return detail
}

func openCodeFailureSummary(stdout string) string {
	for line := range strings.SplitSeq(stdout, "\n") {
		var event struct {
			Type string `json:"type"`
			Part struct {
				Text string `json:"text"`
			} `json:"part"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Type == "text" && strings.TrimSpace(event.Part.Text) != "" {
			return openCodeSummary(stdout)
		}
	}
	return ""
}

func providerErrorDetail(event openCodeErrorEvent) *engineruntime.ProviderErrorDetail {
	detail := &engineruntime.ProviderErrorDetail{
		Message:    redact.OperatorText(event.Error.Data.Message),
		ProviderID: redact.OperatorText(event.Error.Data.ProviderID),
		Code:       providerErrorCode(event.Error.Data.ResponseBody),
	}
	if event.Error.Data.StatusCode != nil {
		detail.StatusCode = *event.Error.Data.StatusCode
	}
	return detail
}

func providerErrorCode(responseBody string) string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(responseBody), &body) != nil {
		return ""
	}
	code := redact.OperatorText(body.Error.Code)
	if len(code) > maxProviderErrorCodeBytes {
		return textutil.Truncate(code, maxProviderErrorCodeBytes-len("…"))
	}
	return code
}

func providerRejectionReason(statusCode int) string {
	switch statusCode {
	case 401:
		return "model provider rejected the sandbox credential (HTTP 401)"
	case 403:
		return "model provider refused the sandbox request (HTTP 403)"
	default:
		return fmt.Sprintf("model provider rejected the sandbox credential%s", providerStatusSuffix(statusCode))
	}
}

func providerStatusSuffix(statusCode int) string {
	if statusCode == 0 {
		return ""
	}
	return fmt.Sprintf(" (HTTP %d)", statusCode)
}
