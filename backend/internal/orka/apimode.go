package orka

import (
	"fmt"
	"strings"
)

const (
	// APIModeAuto accepts the API selected by the Orka provider.
	APIModeAuto = "auto"
	// APIModeResponses requires the OpenAI Responses API.
	APIModeResponses = "responses"
	// APIModeChatCompletions requires the OpenAI Chat Completions API.
	APIModeChatCompletions = "chat_completions"

	// APIModeAnnotation records the expected provider API on generated Tasks.
	APIModeAnnotation = "orka.dashboard/api-mode"
)

// NormalizeAPIMode validates and normalizes an Orka provider API mode.
func NormalizeAPIMode(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = APIModeAuto
	}
	switch value {
	case APIModeAuto, APIModeResponses, APIModeChatCompletions:
		return value, nil
	default:
		return "", fmt.Errorf("unsupported Orka API mode %q (want %q, %q, or %q)", value, APIModeAuto, APIModeResponses, APIModeChatCompletions)
	}
}

// ValidateObservedAPIMode verifies the API reported by the Orka worker.
func ValidateObservedAPIMode(expected, observed string) error {
	expected, err := NormalizeAPIMode(expected)
	if err != nil {
		return err
	}
	observed = strings.ToLower(strings.TrimSpace(observed))
	if observed == "" {
		return fmt.Errorf("model request telemetry did not report an API mode")
	}
	if observed == "mixed" {
		return fmt.Errorf("model requests used multiple API modes")
	}
	if _, err := NormalizeAPIMode(observed); err != nil || observed == APIModeAuto {
		return fmt.Errorf("model request telemetry reported unsupported API mode %q", observed)
	}
	if expected != APIModeAuto && observed != expected {
		return fmt.Errorf("model requests used %s, expected %s", observed, expected)
	}
	return nil
}
