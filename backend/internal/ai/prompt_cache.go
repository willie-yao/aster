package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
)

const analysisPromptCacheName = "aster_analysis_v1"

type promptCacheKeyContextKey struct{}

func analysisPromptCacheKey(stablePrompt string, schemas []tools.Schema) string {
	promptSum := sha256.Sum256([]byte(stablePrompt))
	schemaJSON, _ := json.Marshal(schemas)
	schemaSum := sha256.Sum256(schemaJSON)
	return analysisPromptCacheName + ":" + hex.EncodeToString(promptSum[:8]) + ":" + hex.EncodeToString(schemaSum[:8])
}

func withPromptCacheKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, promptCacheKeyContextKey{}, key)
}

func promptCacheKeyFromContext(ctx context.Context) string {
	key, _ := ctx.Value(promptCacheKeyContextKey{}).(string)
	return key
}
