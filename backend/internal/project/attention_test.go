package project

import (
	"math"
	"strings"
	"testing"
)

func floatPtr(v float64) *float64 { return &v }

func TestEffectiveAttention_Defaults(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		var c *Config
		if got := c.EffectiveAttention().PersistentAfter; got != defaultPersistentAfter {
			t.Errorf("PersistentAfter = %d, want %d", got, defaultPersistentAfter)
		}
	})

	t.Run("omitted section", func(t *testing.T) {
		got := validConfig().EffectiveAttention()
		if got.PersistentAfter != defaultPersistentAfter {
			t.Errorf("PersistentAfter = %d, want %d", got.PersistentAfter, defaultPersistentAfter)
		}
		if got.LowPassRate != nil {
			t.Errorf("LowPassRate = %+v, want nil when unconfigured", got.LowPassRate)
		}
	})

	t.Run("rule guards default", func(t *testing.T) {
		c := validConfig()
		c.Attention = &Attention{LowPassRate: &LowPassRate{Threshold: floatPtr(1)}}
		got := c.EffectiveAttention()
		if got.LowPassRate == nil {
			t.Fatal("LowPassRate = nil, want the configured rule")
		}
		if got.LowPassRate.MinRuns != defaultLowPassRateMinRuns {
			t.Errorf("MinRuns = %d, want %d", got.LowPassRate.MinRuns, defaultLowPassRateMinRuns)
		}
		if got.LowPassRate.MaxItems != defaultLowPassRateMaxItems {
			t.Errorf("MaxItems = %d, want %d", got.LowPassRate.MaxItems, defaultLowPassRateMaxItems)
		}
		if got.LowPassRate.RecentRuns != 0 {
			t.Errorf("RecentRuns = %d, want 0 (full window)", got.LowPassRate.RecentRuns)
		}
		// Defaulting must not write back into the loaded config.
		if c.Attention.LowPassRate.MinRuns != 0 {
			t.Errorf("EffectiveAttention mutated the source config: %+v", c.Attention.LowPassRate)
		}
	})

	t.Run("explicit values win", func(t *testing.T) {
		c := validConfig()
		c.Attention = &Attention{
			PersistentAfter: 5,
			LowPassRate:     &LowPassRate{Threshold: floatPtr(0.8), MinRuns: 2, RecentRuns: 10, MaxItems: 7},
		}
		got := c.EffectiveAttention()
		if got.PersistentAfter != 5 {
			t.Errorf("PersistentAfter = %d, want 5", got.PersistentAfter)
		}
		if *got.LowPassRate.Threshold != 0.8 || got.LowPassRate.MinRuns != 2 ||
			got.LowPassRate.RecentRuns != 10 || got.LowPassRate.MaxItems != 7 {
			t.Errorf("rule = %+v, want the configured values", got.LowPassRate)
		}
	})
}

func TestValidate_Attention(t *testing.T) {
	cases := []struct {
		name      string
		attention *Attention
		wantErr   string
	}{
		{
			name:      "valid full cutoff",
			attention: &Attention{PersistentAfter: 2, LowPassRate: &LowPassRate{Threshold: floatPtr(1)}},
		},
		{
			name:      "valid zero cutoff",
			attention: &Attention{LowPassRate: &LowPassRate{Threshold: floatPtr(0)}},
		},
		{
			name:      "threshold required",
			attention: &Attention{LowPassRate: &LowPassRate{}},
			wantErr:   "attention.low_pass_rate.threshold is required",
		},
		{
			name:      "threshold above one",
			attention: &Attention{LowPassRate: &LowPassRate{Threshold: floatPtr(1.5)}},
			wantErr:   "attention.low_pass_rate.threshold must be between 0 and 1",
		},
		{
			name:      "threshold below zero",
			attention: &Attention{LowPassRate: &LowPassRate{Threshold: floatPtr(-0.1)}},
			wantErr:   "attention.low_pass_rate.threshold must be between 0 and 1",
		},
		{
			name:      "threshold NaN",
			attention: &Attention{LowPassRate: &LowPassRate{Threshold: floatPtr(math.NaN())}},
			wantErr:   "attention.low_pass_rate.threshold must be between 0 and 1",
		},
		{
			name:      "negative persistent_after",
			attention: &Attention{PersistentAfter: -1},
			wantErr:   "attention.persistent_after must not be negative",
		},
		{
			name:      "negative min_runs",
			attention: &Attention{LowPassRate: &LowPassRate{Threshold: floatPtr(1), MinRuns: -1}},
			wantErr:   "attention.low_pass_rate.min_runs must not be negative",
		},
		{
			name:      "negative recent_runs",
			attention: &Attention{LowPassRate: &LowPassRate{Threshold: floatPtr(1), RecentRuns: -1}},
			wantErr:   "attention.low_pass_rate.recent_runs must not be negative",
		},
		{
			name:      "negative max_items",
			attention: &Attention{LowPassRate: &LowPassRate{Threshold: floatPtr(1), MaxItems: -1}},
			wantErr:   "attention.low_pass_rate.max_items must not be negative",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.Attention = tc.attention
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("valid attention config rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestAttention_RoundTripsThroughYAML(t *testing.T) {
	cfg, err := parse(strings.NewReader(`
id: test
name: Test
discovery:
  testgrid_dashboard: test-dashboard
storage:
  provider: gcs
  bucket: test-bucket
branding:
  title: Test
  base_path: /test
  site_url: https://example.com
  source_repo:
    owner: owner
    name: name
attention:
  persistent_after: 2
  low_pass_rate:
    threshold: 1.0
    min_runs: 4
    recent_runs: 10
    max_items: 25
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := cfg.EffectiveAttention()
	if got.PersistentAfter != 2 {
		t.Errorf("persistent_after = %d, want 2", got.PersistentAfter)
	}
	if got.LowPassRate == nil {
		t.Fatal("low_pass_rate = nil, want the parsed rule")
	}
	if *got.LowPassRate.Threshold != 1 || got.LowPassRate.MinRuns != 4 ||
		got.LowPassRate.RecentRuns != 10 || got.LowPassRate.MaxItems != 25 {
		t.Errorf("low_pass_rate = %+v, want the parsed values", got.LowPassRate)
	}
}
