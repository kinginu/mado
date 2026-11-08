package usage

import (
	"fmt"
	"strings"
)

// Price is a [[prices]] entry (DESIGN §17.6): [budgets] currency units per million tokens, matched on the
// upstream's message.model. A trailing `*` makes Model a prefix.
type Price struct {
	Model      string  `toml:"model" json:"model"`
	Input      float64 `toml:"input" json:"input"`
	Output     float64 `toml:"output" json:"output"`
	CacheRead  float64 `toml:"cache_read" json:"cache_read"`
	CacheWrite float64 `toml:"cache_write" json:"cache_write"`
}

// Prices is the configured table; exact matches win over prefixes, longer prefixes
// over shorter ones.
type Prices []Price

// Validate rejects empty or negative entries.
func (ps Prices) Validate() error {
	for _, p := range ps {
		if p.Model == "" || p.Model == "*" {
			return fmt.Errorf("prices: model is required")
		}
		if p.Input < 0 || p.Output < 0 || p.CacheRead < 0 || p.CacheWrite < 0 {
			return fmt.Errorf("prices %s: negative price", p.Model)
		}
	}
	return nil
}

// Lookup finds the price of a model.
func (ps Prices) Lookup(model string) (Price, bool) {
	best, bestLen, found := Price{}, -1, false
	for _, p := range ps {
		if prefix, ok := strings.CutSuffix(p.Model, "*"); ok {
			if strings.HasPrefix(model, prefix) && len(prefix) > bestLen {
				best, bestLen, found = p, len(prefix), true
			}
		} else if p.Model == model {
			return p, true
		}
	}
	return best, found
}

// Cost in the [budgets] currency; input excludes the cache parts.
func (p Price) Cost(input, cacheRead, cacheWrite, output int64) float64 {
	return (float64(input)*p.Input + float64(cacheRead)*p.CacheRead + float64(cacheWrite)*p.CacheWrite +
		float64(output)*p.Output) / 1e6
}
