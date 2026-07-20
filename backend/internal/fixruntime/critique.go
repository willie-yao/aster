package fixruntime

import (
	"fmt"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/fixpr"
)

// Critique resolves the configured review gate and fails closed when unavailable.
func Critique(client fixpr.Completer, configured *int) (fixpr.Completer, int, error) {
	retries := 0
	if configured != nil {
		retries = *configured
	}
	if retries > 0 && client == nil {
		return nil, 0, fmt.Errorf("fix critique is configured for %d retry attempt(s), but no AI reviewer is available; configure the AI client or set critique_retries: 0", retries)
	}
	if retries <= 0 {
		return nil, 0, nil
	}
	return client, retries, nil
}
