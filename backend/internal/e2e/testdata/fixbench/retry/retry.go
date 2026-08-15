package retry

import (
	"fmt"
	"strconv"
)

// Parse converts a configured retry count into an integer.
func Parse(value string) (int, error) {
	retries, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse retries: %w", err)
	}
	return retries, nil
}
