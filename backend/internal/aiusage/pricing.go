package aiusage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

const maxRatePerMillion = "1000000"

// Rates are normalized currency prices per one million tokens.
type Rates struct {
	Currency              string
	InputPerMillion       string
	CachedInputPerMillion string
	OutputPerMillion      string
}

// PriceTable applies one immutable pricing snapshot to operations.
type PriceTable struct {
	currency    string
	input       *big.Rat
	cachedInput *big.Rat
	output      *big.Rat
	hash        string
	configured  bool
}

// NewPriceTable validates and normalizes a pricing table. An entirely empty
// table disables cost estimates while retaining token accounting.
func NewPriceTable(rates Rates) (PriceTable, error) {
	rates.Currency = strings.TrimSpace(rates.Currency)
	rates.InputPerMillion = strings.TrimSpace(rates.InputPerMillion)
	rates.CachedInputPerMillion = strings.TrimSpace(rates.CachedInputPerMillion)
	rates.OutputPerMillion = strings.TrimSpace(rates.OutputPerMillion)
	if rates.Currency == "" && rates.InputPerMillion == "" && rates.CachedInputPerMillion == "" && rates.OutputPerMillion == "" {
		return PriceTable{}, nil
	}
	if !validCurrency(rates.Currency) {
		return PriceTable{}, fmt.Errorf("pricing currency must be three ASCII uppercase letters")
	}
	if rates.InputPerMillion == "" || rates.OutputPerMillion == "" {
		return PriceTable{}, fmt.Errorf("pricing requires input_per_million and output_per_million")
	}
	if rates.CachedInputPerMillion == "" {
		rates.CachedInputPerMillion = rates.InputPerMillion
	}
	input, normalizedInput, err := parseRate(rates.InputPerMillion)
	if err != nil {
		return PriceTable{}, fmt.Errorf("input rate: %w", err)
	}
	cached, normalizedCached, err := parseRate(rates.CachedInputPerMillion)
	if err != nil {
		return PriceTable{}, fmt.Errorf("cached input rate: %w", err)
	}
	output, normalizedOutput, err := parseRate(rates.OutputPerMillion)
	if err != nil {
		return PriceTable{}, fmt.Errorf("output rate: %w", err)
	}
	normalized := strings.Join([]string{rates.Currency, normalizedInput, normalizedCached, normalizedOutput}, "\x00")
	digest := sha256.Sum256([]byte(normalized))
	return PriceTable{
		currency: rates.Currency, input: input, cachedInput: cached, output: output,
		hash: hex.EncodeToString(digest[:8]), configured: true,
	}, nil
}

func validCurrency(value string) bool {
	if len(value) != 3 {
		return false
	}
	for _, r := range value {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func parseRate(value string) (*big.Rat, string, error) {
	if value == "" {
		return nil, "", fmt.Errorf("rate is required")
	}
	if strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") || strings.ContainsAny(value, "eE/") {
		return nil, "", fmt.Errorf("rate %q must be a non-negative decimal", value)
	}
	seenDot := false
	for i, r := range value {
		switch {
		case r >= '0' && r <= '9':
		case r == '.' && !seenDot && i > 0 && i < len(value)-1:
			seenDot = true
		default:
			return nil, "", fmt.Errorf("rate %q must be a non-negative decimal", value)
		}
	}
	rate, ok := new(big.Rat).SetString(value)
	if !ok || rate.Sign() < 0 {
		return nil, "", fmt.Errorf("rate %q must be a non-negative decimal", value)
	}
	max, _ := new(big.Rat).SetString(maxRatePerMillion)
	if rate.Cmp(max) > 0 {
		return nil, "", fmt.Errorf("rate %q exceeds %s", value, maxRatePerMillion)
	}
	return rate, normalizeDecimal(value), nil
}

func normalizeDecimal(value string) string {
	parts := strings.SplitN(value, ".", 2)
	integer := strings.TrimLeft(parts[0], "0")
	if integer == "" {
		integer = "0"
	}
	if len(parts) == 1 {
		return integer
	}
	decimal := strings.TrimRight(parts[1], "0")
	if decimal == "" {
		return integer
	}
	return integer + "." + decimal
}

func (p PriceTable) Configured() bool { return p.configured }
func (p PriceTable) Currency() string { return p.currency }
func (p PriceTable) Hash() string     { return p.hash }

// Estimate returns the provider-token cost rounded half-up to currency
// nanounits. Reasoning tokens are already included in output tokens.
func (p PriceTable) Estimate(usage TokenUsage) (int64, error) {
	if !p.configured || !usage.Reported {
		return 0, nil
	}
	if usage.InputTokens < 0 || usage.CachedInputTokens < 0 || usage.OutputTokens < 0 || usage.ReasoningTokens < 0 {
		return 0, fmt.Errorf("token counts must be non-negative")
	}
	if usage.CachedInputTokens > usage.InputTokens {
		return 0, fmt.Errorf("cached input tokens exceed input tokens")
	}
	uncached := int64(usage.InputTokens - usage.CachedInputTokens)
	cost := new(big.Rat)
	cost.Add(cost, scaledTokenCost(uncached, p.input))
	cost.Add(cost, scaledTokenCost(int64(usage.CachedInputTokens), p.cachedInput))
	cost.Add(cost, scaledTokenCost(int64(usage.OutputTokens), p.output))
	return roundPositiveRat(cost)
}

func scaledTokenCost(tokens int64, rate *big.Rat) *big.Rat {
	// Currency nanounits per token are rate_per_million * 1e9 / 1e6.
	scaled := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(1000))
	return new(big.Rat).Mul(new(big.Rat).SetInt(scaled), rate)
}

func roundPositiveRat(value *big.Rat) (int64, error) {
	if value.Sign() < 0 {
		return 0, fmt.Errorf("cost must be non-negative")
	}
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(value.Num(), value.Denom(), remainder)
	if new(big.Int).Lsh(remainder, 1).Cmp(value.Denom()) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, fmt.Errorf("estimated cost exceeds int64 nanounits")
	}
	return quotient.Int64(), nil
}
