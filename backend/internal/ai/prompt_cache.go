package ai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/willie-yao/aster/backend/internal/ai/tools"
)

const analysisPromptCacheName = "aster_analysis_v1"

func analysisPromptCacheKey(stablePrompt string, schemas []tools.Schema) string {
	promptSum := sha256.Sum256([]byte(stablePrompt))
	schemaJSON, _ := json.Marshal(schemas)
	schemaSum := sha256.Sum256(schemaJSON)
	return analysisPromptCacheName + ":" + hex.EncodeToString(promptSum[:8]) + ":" + hex.EncodeToString(schemaSum[:8])
}
