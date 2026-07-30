package project

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

const (
	// AICacheGenerationEnv overrides ai.cache_generation when non-empty.
	AICacheGenerationEnv  = "AI_CACHE_GENERATION"
	maxCacheGenerationLen = 64
)

var cacheGenerationPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateAICacheGeneration validates one operator-selected generation.
func ValidateAICacheGeneration(value string) error {
	if value == "" {
		return nil
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("AI cache generation must not have surrounding whitespace")
	}
	if len(value) > maxCacheGenerationLen {
		return fmt.Errorf("AI cache generation must not exceed %d bytes", maxCacheGenerationLen)
	}
	if !cacheGenerationPattern.MatchString(value) {
		return fmt.Errorf("AI cache generation must start with an alphanumeric character and contain only alphanumerics, dot, underscore, or hyphen")
	}
	return nil
}

// ResolveAICacheGeneration applies the environment-over-project precedence.
func ResolveAICacheGeneration(configValue, envValue string) (string, error) {
	value := configValue
	if strings.TrimSpace(envValue) != "" {
		value = envValue
	}
	if err := ValidateAICacheGeneration(value); err != nil {
		return "", err
	}
	return value, nil
}

// AICacheGenerationFingerprint returns the safe cache-key namespace.
func AICacheGenerationFingerprint(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}
