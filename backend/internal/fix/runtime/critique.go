package runtime

import (
	"fmt"

	fixpr "github.com/willie-yao/aster/backend/internal/fix/pr"
)

// Critique selects the available reviewer and regeneration budget.
func Critique(client fixpr.Completer, configured *int) (fixpr.Completer, int, error) {
	retries := 0
	if configured != nil {
		retries = *configured
	}
	if retries > 0 && client == nil {
		return nil, 0, fmt.Errorf("fix critique is configured for %d retry attempt(s), but no AI reviewer is available; configure the AI client or set critique_retries: 0", retries)
	}
	return client, retries, nil
}
