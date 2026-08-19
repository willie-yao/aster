package fixexecutor

import (
	"encoding/json"
	"fmt"
	"strings"
)

type openCodeErrorEvent struct {
	Type  string `json:"type"`
	Error struct {
		Name string `json:"name"`
		Data struct {
			StatusCode *int `json:"statusCode"`
		} `json:"data"`
	} `json:"error"`
}

// providerCredentialRejection reports the OpenCode error event that means the
// model provider rejected the execution credential. The rejection is permanent
// and operator-fixable, so it is reported separately from runtime trouble.
func providerCredentialRejection(stdout string) (string, bool) {
	for _, line := range strings.Split(stdout, "\n") {
		var event openCodeErrorEvent
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &event) != nil || event.Type != "error" {
			continue
		}
		if event.Error.Name == "ProviderAuthError" {
			return "model provider rejected the sandbox credential", true
		}
		status := event.Error.Data.StatusCode
		if event.Error.Name == "APIError" && status != nil && (*status == 401 || *status == 403) {
			return fmt.Sprintf("model provider rejected the sandbox credential (HTTP %d)", *status), true
		}
	}
	return "", false
}
