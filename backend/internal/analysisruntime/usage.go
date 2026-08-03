package analysisruntime

import (
	"path/filepath"

	"github.com/willie-yao/prow-ai-dashboard/backend/internal/aiusage"
	"github.com/willie-yao/prow-ai-dashboard/backend/internal/project"
)

// NewUsageRecorder creates the configured private usage ledger for one writer.
func NewUsageRecorder(dataDir, filename string, cfg *project.Config) (*aiusage.Recorder, error) {
	if cfg == nil || cfg.AI == nil {
		return nil, nil
	}
	effective := cfg.AI.EffectiveUsage()
	if !effective.Enabled {
		return nil, nil
	}
	pricing, err := aiusage.NewPriceTable(aiusage.Rates{
		Currency:              effective.Pricing.Currency,
		InputPerMillion:       effective.Pricing.InputPerMillion,
		CachedInputPerMillion: effective.Pricing.CachedInputPerMillion,
		OutputPerMillion:      effective.Pricing.OutputPerMillion,
	})
	if err != nil {
		return nil, err
	}
	return aiusage.NewRecorder(filepath.Join(dataDir, filename), aiusage.RecorderOptions{
		RetentionDays: effective.RetentionDays, RecentOperations: effective.RecentOperations, Pricing: pricing,
	})
}
