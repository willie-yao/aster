package fixexecutor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/willie-yao/aster/backend/internal/redact"
	engineruntime "github.com/willie-yao/aster/backend/internal/runtime"
)

type openCodeErrorEvent struct {
	Type  string `json:"type"`
	Error struct {
		Name string `json:"name"`
		Data struct {
			StatusCode *int   `json:"statusCode"`
			Message    string `json:"message"`
			ProviderID string `json:"providerID"`
		} `json:"data"`
	} `json:"error"`
}

// providerCredentialRejection reports the first OpenCode error event that means
// the model provider refused an authenticated execution request.
func providerCredentialRejection(stdout string) (string, *engineruntime.ProviderErrorDetail, bool) {
	for _, line := range strings.Split(stdout, "\n") {
		var event openCodeErrorEvent
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &event) != nil || event.Type != "error" {
			continue
		}
		if event.Error.Name == "ProviderAuthError" {
			detail := providerErrorDetail(event)
			return providerRejectionReason(detail.StatusCode), detail, true
		}
		status := event.Error.Data.StatusCode
		if event.Error.Name == "APIError" && status != nil && (*status == 401 || *status == 403) {
			detail := providerErrorDetail(event)
			return providerRejectionReason(detail.StatusCode), detail, true
		}
	}
	return "", nil, false
}

func providerErrorDetail(event openCodeErrorEvent) *engineruntime.ProviderErrorDetail {
	detail := &engineruntime.ProviderErrorDetail{
		Message:    redact.OperatorText(event.Error.Data.Message),
		ProviderID: redact.OperatorText(event.Error.Data.ProviderID),
	}
	if event.Error.Data.StatusCode != nil {
		detail.StatusCode = *event.Error.Data.StatusCode
	}
	return detail
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
