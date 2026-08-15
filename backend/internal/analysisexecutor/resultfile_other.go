//go:build !linux && !darwin

package analysisexecutor

import "fmt"

func readSingleResultFile(string, string, int64) (string, error) {
	return "", fmt.Errorf("analysis executor result retrieval is supported only on Linux and macOS")
}
