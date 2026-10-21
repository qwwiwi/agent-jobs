package jobs

import (
	"fmt"
	"math"
	"regexp"
	"strings"
)

// ModelPrice is a model's list price in USD per million tokens. CacheRead prices cached input
// (claude cache reads, codex cached input); CacheWrite prices claude cache writes (5-minute TTL).
type ModelPrice struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// TokenUsage is the token totals of one or more attempts. For codex, Input excludes the cached
// input tokens, which count as CacheRead.
type TokenUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// Valid reports whether no count of u is negative.
func (u TokenUsage) Valid() bool {
	return u.Input >= 0 && u.Output >= 0 && u.CacheRead >= 0 && u.CacheWrite >= 0
}

// Add returns the sum of u and o; ok is false when a count is negative (that field sums to 0) or a sum
// overflows int64 (that field saturates at math.MaxInt64).
func (u TokenUsage) Add(o TokenUsage) (sum TokenUsage, ok bool) {
	ok = true
	add := func(a, b int64) int64 {
		switch {
		case a < 0 || b < 0:
			ok = false
			return 0
		case a > math.MaxInt64-b:
			ok = false
			return math.MaxInt64
		}
		return a + b
	}
	sum = TokenUsage{add(u.Input, o.Input), add(u.Output, o.Output), add(u.CacheRead, o.CacheRead),
		add(u.CacheWrite, o.CacheWrite)}
	return sum, ok
}

// Cost prices u at p.
func (p ModelPrice) Cost(u TokenUsage) float64 {
	return (float64(u.Input)*p.Input + float64(u.Output)*p.Output + float64(u.CacheRead)*p.CacheRead +
		float64(u.CacheWrite)*p.CacheWrite) / 1e6
}

// listPrices are the Anthropic first-party list prices (USD per MTok, 2026-10) by normalized model
// name (normalizeModel): the CLI aliases name the current model of each family. Codex (gpt-*) models
// have no built-in row: config model_prices prices them, else their cost stays unknown.
var listPrices = map[string]ModelPrice{
	"fable":      {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"fable-5-1":  {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"fable-5":    {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	"opus":       {Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5},
	"opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5},
	"opus-5":     {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"opus-4-8":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"opus-4-7":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"opus-4-6":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"opus-4-5":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"opus-4-1":   {Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75},
	"opus-4":     {Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75},
	"opus-4-0":   {Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75},
	"sonnet":     {Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5},
	"sonnet-5-5": {Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5},
	"sonnet-5":   {Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5},
	"sonnet-4-6": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	"sonnet-4-5": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	"sonnet-4":   {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	"sonnet-4-0": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	"haiku":      {Input: 0.1, Output: 0.5, CacheRead: 0.01, CacheWrite: 0.125},
	"haiku-5-5":  {Input: 0.1, Output: 0.5, CacheRead: 0.01, CacheWrite: 0.125},
	"haiku-4-5":  {Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
}

// reDateSuffix is a model id's snapshot date (claude-opus-4-1-20250805).
var reDateSuffix = regexp.MustCompile(`-\d{8}$`)

// normalizeModel maps a model name or alias to its price key: lower case, without a context suffix
// ("[1m]"), the "claude-" prefix or a snapshot date.
func normalizeModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.IndexByte(m, '['); i >= 0 {
		m = m[:i]
	}
	m = strings.TrimPrefix(m, "claude-")
	return reDateSuffix.ReplaceAllString(m, "")
}

// PriceFor returns the price of model: config model_prices first (by the exact name, then the
// normalized one), then the built-in list prices. ok is false for a model with no price row.
func (c Config) PriceFor(model string) (ModelPrice, bool) {
	if p, ok := c.ModelPrices[model]; ok {
		return p, true
	}
	n := normalizeModel(model)
	if p, ok := c.ModelPrices[n]; ok {
		return p, true
	}
	p, ok := listPrices[n]
	return p, ok
}

// validatePrices rejects a negative price in model_prices.
func validatePrices(prices map[string]ModelPrice) error {
	for m, p := range prices {
		if p.Input < 0 || p.Output < 0 || p.CacheRead < 0 || p.CacheWrite < 0 {
			return fmt.Errorf("model_prices %q: prices must be 0 or positive (USD per million tokens)", m)
		}
	}
	return nil
}

// EstimatedCostText is an estimated cost for people: "$0.42 est.".
func EstimatedCostText(cost float64) string { return fmt.Sprintf("$%.2f est.", cost) }

// streamTokenCounter counts only usage received from runner stdout. Repeated Claude message IDs
// contribute their largest usage; Codex turn usage is additive.
type streamTokenCounter struct {
	byID    map[string]TokenUsage
	total   TokenUsage
	invalid bool
}

func (c *streamTokenCounter) apply(ev statsEvent) {
	var usage TokenUsage
	if ev.Type == "turn.completed" && ev.Usage != nil {
		u := ev.Usage
		if u.CachedInputTokens < 0 || u.CachedInputTokens > u.InputTokens {
			c.invalid = true
			return
		}
		usage = TokenUsage{Input: u.InputTokens - u.CachedInputTokens, Output: u.OutputTokens, CacheRead: u.CachedInputTokens}
	} else if ev.Type == "assistant" && ev.Message != nil && ev.Message.Usage != nil {
		u := ev.Message.Usage
		usage = TokenUsage{Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens}
		if !usage.Valid() {
			c.invalid = true
			return
		}
		if id := ev.Message.ID; id != "" {
			if c.byID == nil {
				c.byID = map[string]TokenUsage{}
			}
			old := c.byID[id]
			next := TokenUsage{max(old.Input, usage.Input), max(old.Output, usage.Output), max(old.CacheRead, usage.CacheRead), max(old.CacheWrite, usage.CacheWrite)}
			c.byID[id] = next
			usage = TokenUsage{next.Input - old.Input, next.Output - old.Output, next.CacheRead - old.CacheRead, next.CacheWrite - old.CacheWrite}
		}
	} else {
		return
	}
	var ok bool
	c.total, ok = c.total.Add(usage)
	c.invalid = c.invalid || !ok
}
