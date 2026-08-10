package aiusage

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"testing"
)

func TestNewPriceTable(t *testing.T) {
	tests := []struct {
		name    string
		rates   Rates
		wantErr bool
	}{
		{name: "empty"},
		{name: "valid", rates: Rates{Currency: "USD", InputPerMillion: "1.2500", OutputPerMillion: "10"}},
		{name: "cached", rates: Rates{Currency: "USD", InputPerMillion: "1", CachedInputPerMillion: "0.1", OutputPerMillion: "2"}},
		{name: "cache write", rates: Rates{Currency: "USD", InputPerMillion: "1", CachedInputPerMillion: "0.1", CacheWriteInputPerMillion: "1.25", OutputPerMillion: "2"}},
		{name: "lower currency", rates: Rates{Currency: "usd", InputPerMillion: "1", OutputPerMillion: "2"}, wantErr: true},
		{name: "numeric currency", rates: Rates{Currency: "123", InputPerMillion: "1", OutputPerMillion: "2"}, wantErr: true},
		{name: "symbol currency", rates: Rates{Currency: "$$$", InputPerMillion: "1", OutputPerMillion: "2"}, wantErr: true},
		{name: "missing output", rates: Rates{Currency: "USD", InputPerMillion: "1"}, wantErr: true},
		{name: "negative", rates: Rates{Currency: "USD", InputPerMillion: "-1", OutputPerMillion: "2"}, wantErr: true},
		{name: "exponent", rates: Rates{Currency: "USD", InputPerMillion: "1e2", OutputPerMillion: "2"}, wantErr: true},
		{name: "fraction", rates: Rates{Currency: "USD", InputPerMillion: "1/2", OutputPerMillion: "2"}, wantErr: true},
		{name: "too large", rates: Rates{Currency: "USD", InputPerMillion: "1000000.1", OutputPerMillion: "2"}, wantErr: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			table, err := NewPriceTable(testCase.rates)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("NewPriceTable() error = %v, wantErr %v", err, testCase.wantErr)
			}
			if err == nil && testCase.name == "valid" {
				if !table.Configured() || table.Currency() != "USD" || len(table.Hash()) != 16 {
					t.Fatalf("table = %+v", table)
				}
			}
			if err == nil && testCase.name == "empty" && table.Configured() {
				t.Fatal("empty table is configured")
			}
		})
	}
}

func TestPriceTableHashPreservesLegacyRatesWithoutCacheWrite(t *testing.T) {
	table, err := NewPriceTable(Rates{Currency: "USD", InputPerMillion: "1.25", CachedInputPerMillion: "0.125", OutputPerMillion: "10"})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{"USD", "1.25", "0.125", "10"}, "\x00")))
	if want := hex.EncodeToString(digest[:8]); table.Hash() != want {
		t.Fatalf("hash = %q, want legacy %q", table.Hash(), want)
	}
}

func TestPriceTableHashChangesForCacheWriteRate(t *testing.T) {
	base, err := NewPriceTable(Rates{Currency: "USD", InputPerMillion: "1.25", CachedInputPerMillion: "0.125", OutputPerMillion: "10"})
	if err != nil {
		t.Fatal(err)
	}
	withWrite, err := NewPriceTable(Rates{Currency: "USD", InputPerMillion: "1.25", CachedInputPerMillion: "0.125", CacheWriteInputPerMillion: "1.5", OutputPerMillion: "10"})
	if err != nil {
		t.Fatal(err)
	}
	if withWrite.Hash() == base.Hash() {
		t.Fatalf("cache write rate did not change pricing hash %q", base.Hash())
	}
}

func TestPriceTableEstimate(t *testing.T) {
	table, err := NewPriceTable(Rates{
		Currency: "USD", InputPerMillion: "1", CachedInputPerMillion: "0.1", CacheWriteInputPerMillion: "1.25", OutputPerMillion: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := table.Estimate(TokenUsage{
		Reported: true, InputTokens: 1000, CachedInputTokens: 400,
		CacheWriteInputTokens: 100, CacheWriteInputTokensReported: true,
		OutputTokens: 250, ReasoningTokens: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 500 * $1/M + 400 * $0.1/M + 100 * $1.25/M + 250 * $2/M = $0.001165.
	if got != 1_165_000 {
		t.Fatalf("cost nanos = %d, want 1165000", got)
	}
}

func TestPriceTableEstimateOmitsUnconfiguredCacheWriteRate(t *testing.T) {
	table, err := NewPriceTable(Rates{Currency: "USD", InputPerMillion: "1", CachedInputPerMillion: "0.1", OutputPerMillion: "2"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := table.Estimate(TokenUsage{
		Reported: true, InputTokens: 1000, CachedInputTokens: 400,
		CacheWriteInputTokens: 100, CacheWriteInputTokensReported: true, OutputTokens: 250,
	})
	if err != nil {
		t.Fatal(err)
	}
	if table.CacheWriteConfigured() || got != 1_040_000 {
		t.Fatalf("cache write configured=%t cost=%d", table.CacheWriteConfigured(), got)
	}
}

func TestPriceTableEstimateRoundsHalfUp(t *testing.T) {
	table, err := NewPriceTable(Rates{Currency: "USD", InputPerMillion: "0.0005", OutputPerMillion: "0"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := table.Estimate(TokenUsage{Reported: true, InputTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("cost nanos = %d, want 1", got)
	}
}

func TestPriceTableEstimateValidation(t *testing.T) {
	table, err := NewPriceTable(Rates{Currency: "USD", InputPerMillion: "1", OutputPerMillion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := table.Estimate(TokenUsage{Reported: true, InputTokens: 1, CachedInputTokens: 2}); err == nil {
		t.Fatal("expected cached-input validation error")
	}
	if _, err := table.Estimate(TokenUsage{Reported: true, InputTokens: 2, CachedInputTokens: 1, CacheWriteInputTokens: 2, CacheWriteInputTokensReported: true}); err == nil {
		t.Fatal("expected cache-write validation error")
	}
	if _, err := table.Estimate(TokenUsage{Reported: true, InputTokens: -1}); err == nil {
		t.Fatal("expected negative-token validation error")
	}
	if _, err := table.Estimate(TokenUsage{Reported: true, InputTokens: math.MaxInt, OutputTokens: math.MaxInt}); err == nil {
		t.Fatal("expected cost overflow error")
	}
}
