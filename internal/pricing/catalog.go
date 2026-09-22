package pricing

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

const (
	DefaultCurrency    = "USD"
	UnknownUseFallback = "fallback"
	UnknownReject      = "reject"
)

var ErrUnknownPrice = errors.New("no provider/model price configured")

// Rule is a provider/model price entry. Rates are expressed per one million
// tokens, matching the way most provider price sheets are published.
type Rule struct {
	Provider                     string
	Model                        string
	InputPerMillionTokensUSD     float64
	OutputPerMillionTokensUSD    float64
	EmbeddingPerMillionTokensUSD float64
}

// Estimate is a cost calculation plus provenance for observability.
type Estimate struct {
	USD      float64
	Known    bool
	Currency string
	Version  string
}

// Catalog is immutable after construction and safe to share between request
// handlers. Replacing the pointer in budget.Manager makes reload atomic.
type Catalog struct {
	rules    []Rule
	fallback float64
	currency string
	version  string
	unknown  string
}

func NewCatalog(rules []Rule, fallback float64, currency, version, unknownPolicy string) (*Catalog, error) {
	if fallback < 0 {
		return nil, fmt.Errorf("fallback cost must be >= 0")
	}
	if currency == "" {
		currency = DefaultCurrency
	}
	if !strings.EqualFold(currency, DefaultCurrency) {
		return nil, fmt.Errorf("unsupported pricing currency %q: only %s is supported", currency, DefaultCurrency)
	}
	if unknownPolicy == "" {
		unknownPolicy = UnknownUseFallback
	}
	if unknownPolicy != UnknownUseFallback && unknownPolicy != UnknownReject {
		return nil, fmt.Errorf("unknown pricing policy %q: want %q or %q", unknownPolicy, UnknownUseFallback, UnknownReject)
	}
	copyRules := append([]Rule(nil), rules...)
	for i, rule := range copyRules {
		if rule.Provider == "" || rule.Model == "" {
			return nil, fmt.Errorf("pricing rule %d requires provider and model", i)
		}
		if rule.InputPerMillionTokensUSD < 0 || rule.OutputPerMillionTokensUSD < 0 || rule.EmbeddingPerMillionTokensUSD < 0 {
			return nil, fmt.Errorf("pricing rule %d contains a negative rate", i)
		}
		if rule.InputPerMillionTokensUSD == 0 && rule.OutputPerMillionTokensUSD == 0 && rule.EmbeddingPerMillionTokensUSD == 0 {
			return nil, fmt.Errorf("pricing rule %d must define at least one positive rate", i)
		}
	}
	sort.SliceStable(copyRules, func(i, j int) bool {
		if copyRules[i].Provider != copyRules[j].Provider {
			return copyRules[i].Provider < copyRules[j].Provider
		}
		return copyRules[i].Model < copyRules[j].Model
	})
	return &Catalog{rules: copyRules, fallback: fallback, currency: currency, version: version, unknown: unknownPolicy}, nil
}

func (c *Catalog) Chat(provider, model string, promptTokens, completionTokens int) (Estimate, error) {
	if promptTokens < 0 {
		promptTokens = 0
	}
	if completionTokens < 0 {
		completionTokens = 0
	}
	if rule, ok := c.bestRule(provider, model, false); ok {
		return Estimate{
			USD:      perMillion(promptTokens, rule.InputPerMillionTokensUSD) + perMillion(completionTokens, rule.OutputPerMillionTokensUSD),
			Known:    true,
			Currency: c.currency,
			Version:  c.version,
		}, nil
	}
	return c.unknownEstimate(promptTokens + completionTokens)
}

// EstimateChat uses the highest matching configured rate when the eventual
// provider is not known yet. This prevents a fallback to a more expensive
// provider from silently under-reserving a hard budget.
func (c *Catalog) EstimateChat(model string, promptTokens, completionTokens int) (Estimate, error) {
	var best *Rule
	for i := range c.rules {
		if !modelMatches(c.rules[i].Model, model) {
			continue
		}
		if best == nil || chatRate(&c.rules[i]) > chatRate(best) {
			best = &c.rules[i]
		}
	}
	if best != nil {
		return Estimate{
			USD:      perMillion(promptTokens, best.InputPerMillionTokensUSD) + perMillion(completionTokens, best.OutputPerMillionTokensUSD),
			Known:    true,
			Currency: c.currency,
			Version:  c.version,
		}, nil
	}
	return c.unknownEstimate(promptTokens + completionTokens)
}

func (c *Catalog) Embedding(provider, model string, tokens int) (Estimate, error) {
	if tokens < 0 {
		tokens = 0
	}
	if rule, ok := c.bestRule(provider, model, true); ok {
		return Estimate{USD: perMillion(tokens, rule.EmbeddingPerMillionTokensUSD), Known: true, Currency: c.currency, Version: c.version}, nil
	}
	return c.unknownEstimate(tokens)
}

func (c *Catalog) unknownEstimate(tokens int) (Estimate, error) {
	if c.unknown == UnknownReject {
		return Estimate{Currency: c.currency, Version: c.version}, fmt.Errorf("%w: unknown model", ErrUnknownPrice)
	}
	return Estimate{USD: perMillion(tokens, c.fallback), Known: false, Currency: c.currency, Version: c.version}, nil
}

func (c *Catalog) bestRule(provider, model string, embedding bool) (*Rule, bool) {
	var best *Rule
	for i := range c.rules {
		rule := &c.rules[i]
		if provider != "" && rule.Provider != provider {
			continue
		}
		if !modelMatches(rule.Model, model) {
			continue
		}
		rate := rule.EmbeddingPerMillionTokensUSD
		if !embedding {
			rate = chatRate(rule)
		}
		if rate <= 0 {
			continue
		}
		if best == nil || ruleRank(rule, model) > ruleRank(best, model) {
			best = rule
		}
	}
	return best, best != nil
}

func ruleRank(rule *Rule, model string) int {
	if rule.Model == model {
		return 2
	}
	return 1
}

func modelMatches(pattern, model string) bool {
	if pattern == model {
		return true
	}
	matched, err := path.Match(pattern, model)
	return err == nil && matched
}

func chatRate(rule *Rule) float64 {
	return rule.InputPerMillionTokensUSD + rule.OutputPerMillionTokensUSD
}

func perMillion(tokens int, rate float64) float64 {
	return float64(tokens) * rate / 1_000_000
}
