package pricing

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	DefaultCurrency    = "USD"
	UnknownUseFallback = "fallback"
	UnknownReject      = "reject"

	tokensPerMillion = 1_000_000
)

var ErrUnknownPrice = errors.New("no provider/model price configured")

// Rule is a provider/model price entry. Rates are expressed per one million
// tokens, matching the way most provider price sheets are published.
//
// CacheReadPerMillionTokensUSD and CacheWritePerMillionTokensUSD are optional.
// A zero value means "not configured" and those tokens are billed at the
// regular input rate. They are only used by ChatWithCacheTokens /
// EstimateChatWithCacheTokens; Chat keeps billing every prompt token as input.
type Rule struct {
	Provider                      string
	Model                         string
	InputPerMillionTokensUSD      float64
	OutputPerMillionTokensUSD     float64
	EmbeddingPerMillionTokensUSD  float64
	CacheReadPerMillionTokensUSD  float64
	CacheWritePerMillionTokensUSD float64
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
	rules    []compiledRule
	fallback float64 // USD per million tokens
	currency string
	version  string
	unknown  string
}

// compiledRule precomputes the glob tokens and specificity score so request
// paths never re-parse provider model patterns.
type compiledRule struct {
	rule   Rule
	tokens []globToken
	score  ruleScore
}

// NewCatalog builds a catalog whose fallback is expressed in USD per one
// million tokens. This is the price-sheet convention and the historical
// behavior of this constructor.
//
// Callers that store a fallback as USD per token (for example a
// cost_per_token_usd setting) must use NewCatalogWithFallbackPerToken.
func NewCatalog(rules []Rule, fallbackPerMillionTokensUSD float64, currency, version, unknownPolicy string) (*Catalog, error) {
	return newCatalog(rules, fallbackPerMillionTokensUSD, currency, version, unknownPolicy)
}

// NewCatalogWithFallbackPerToken builds a catalog from a fallback expressed in
// USD per token, which is how the gateway's cost_per_token_usd setting is
// written. The value is converted once to the per-million representation used
// internally: 0.00001 USD/token becomes 10 USD per 1M tokens.
//
// This constructor exists so cmd/gateway can pass cost_per_token_usd without
// silently under-pricing unknown models by a factor of one million.
func NewCatalogWithFallbackPerToken(rules []Rule, fallbackPerTokenUSD float64, currency, version, unknownPolicy string) (*Catalog, error) {
	if fallbackPerTokenUSD < 0 {
		return nil, fmt.Errorf("fallback cost must be >= 0")
	}
	return newCatalog(rules, fallbackPerTokenUSD*tokensPerMillion, currency, version, unknownPolicy)
}

func newCatalog(rules []Rule, fallbackPerMillionTokensUSD float64, currency, version, unknownPolicy string) (*Catalog, error) {
	if fallbackPerMillionTokensUSD < 0 {
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
	compiled := make([]compiledRule, len(rules))
	for i, rule := range rules {
		if rule.Provider == "" || rule.Model == "" {
			return nil, fmt.Errorf("pricing rule %d requires provider and model", i)
		}
		if rule.InputPerMillionTokensUSD < 0 || rule.OutputPerMillionTokensUSD < 0 ||
			rule.EmbeddingPerMillionTokensUSD < 0 || rule.CacheReadPerMillionTokensUSD < 0 ||
			rule.CacheWritePerMillionTokensUSD < 0 {
			return nil, fmt.Errorf("pricing rule %d contains a negative rate", i)
		}
		if rule.InputPerMillionTokensUSD == 0 && rule.OutputPerMillionTokensUSD == 0 &&
			rule.EmbeddingPerMillionTokensUSD == 0 && rule.CacheReadPerMillionTokensUSD == 0 &&
			rule.CacheWritePerMillionTokensUSD == 0 {
			return nil, fmt.Errorf("pricing rule %d must define at least one positive rate", i)
		}
		tokens, ok := parseGlob(rule.Model)
		if !ok {
			return nil, fmt.Errorf("pricing rule %d has an invalid model pattern %q", i, rule.Model)
		}
		compiled[i] = compiledRule{rule: rule, tokens: tokens, score: scorePattern(tokens, rule.Model)}
	}
	sort.SliceStable(compiled, func(i, j int) bool {
		if compiled[i].rule.Provider != compiled[j].rule.Provider {
			return compiled[i].rule.Provider < compiled[j].rule.Provider
		}
		return compiled[i].rule.Model < compiled[j].rule.Model
	})
	return &Catalog{rules: compiled, fallback: fallbackPerMillionTokensUSD, currency: currency, version: version, unknown: unknownPolicy}, nil
}

// FallbackPerMillionTokensUSD returns the configured fallback rate.
func (c *Catalog) FallbackPerMillionTokensUSD() float64 { return c.fallback }

// FallbackPerTokenUSD returns the configured fallback expressed per token.
func (c *Catalog) FallbackPerTokenUSD() float64 { return c.fallback / tokensPerMillion }

func (c *Catalog) Chat(provider, model string, promptTokens, completionTokens int) (Estimate, error) {
	if promptTokens < 0 {
		promptTokens = 0
	}
	if completionTokens < 0 {
		completionTokens = 0
	}
	if rule, ok := c.bestRule(provider, model, chatRates); ok {
		return Estimate{
			USD:      perMillion(promptTokens, rule.InputPerMillionTokensUSD) + perMillion(completionTokens, rule.OutputPerMillionTokensUSD),
			Known:    true,
			Currency: c.currency,
			Version:  c.version,
		}, nil
	}
	return c.unknownEstimate(promptTokens + completionTokens)
}

// ChatWithCacheTokens prices a chat request whose prompt usage includes
// provider-reported cache hits/writes. cacheReadTokens and cacheWriteTokens
// are subsets of promptTokens; they are billed at the rule's cache rates when
// configured, or at the regular input rate otherwise. Values that do not fit
// inside promptTokens are clamped so a malformed provider usage block cannot
// produce a negative estimate.
func (c *Catalog) ChatWithCacheTokens(provider, model string, promptTokens, completionTokens, cacheReadTokens, cacheWriteTokens int) (Estimate, error) {
	uncached, cacheRead, cacheWrite := splitCacheTokens(promptTokens, cacheReadTokens, cacheWriteTokens)
	completionTokens = clampNonNegative(completionTokens)
	if rule, ok := c.bestRule(provider, model, cachedRates); ok {
		return Estimate{
			USD:      cachedChatCost(rule, uncached, completionTokens, cacheRead, cacheWrite),
			Known:    true,
			Currency: c.currency,
			Version:  c.version,
		}, nil
	}
	return c.unknownEstimate(uncached + cacheRead + cacheWrite + completionTokens)
}

// EstimateChat uses the most specific matching rule when the eventual provider
// is not known yet. Ties between equally specific rules use the highest rate,
// which prevents a fallback to a more expensive provider from silently
// under-reserving a hard budget.
func (c *Catalog) EstimateChat(model string, promptTokens, completionTokens int) (Estimate, error) {
	promptTokens = clampNonNegative(promptTokens)
	completionTokens = clampNonNegative(completionTokens)
	if best, ok := c.estimateRule(model, chatRates, promptTokens, completionTokens, 0, 0); ok {
		return Estimate{
			USD:      perMillion(promptTokens, best.InputPerMillionTokensUSD) + perMillion(completionTokens, best.OutputPerMillionTokensUSD),
			Known:    true,
			Currency: c.currency,
			Version:  c.version,
		}, nil
	}
	return c.unknownEstimate(promptTokens + completionTokens)
}

// EstimateChatWithCacheTokens is the cache-aware counterpart of EstimateChat.
func (c *Catalog) EstimateChatWithCacheTokens(model string, promptTokens, completionTokens, cacheReadTokens, cacheWriteTokens int) (Estimate, error) {
	uncached, cacheRead, cacheWrite := splitCacheTokens(promptTokens, cacheReadTokens, cacheWriteTokens)
	completionTokens = clampNonNegative(completionTokens)
	if best, ok := c.estimateRule(model, cachedRates, uncached, completionTokens, cacheRead, cacheWrite); ok {
		return Estimate{
			USD:      cachedChatCost(best, uncached, completionTokens, cacheRead, cacheWrite),
			Known:    true,
			Currency: c.currency,
			Version:  c.version,
		}, nil
	}
	return c.unknownEstimate(uncached + cacheRead + cacheWrite + completionTokens)
}

func (c *Catalog) Embedding(provider, model string, tokens int) (Estimate, error) {
	if tokens < 0 {
		tokens = 0
	}
	if rule, ok := c.bestRule(provider, model, embeddingRates); ok {
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

type rateSet int

const (
	chatRates rateSet = iota
	cachedRates
	embeddingRates
)

// rate reports the rate used to decide whether a rule applies and, when no
// other tie-breaker applies, which duplicate is more conservative.
func (s rateSet) rate(rule *Rule) float64 {
	switch s {
	case embeddingRates:
		return rule.EmbeddingPerMillionTokensUSD
	case cachedRates:
		return rule.InputPerMillionTokensUSD + rule.OutputPerMillionTokensUSD +
			rule.CacheReadPerMillionTokensUSD + rule.CacheWritePerMillionTokensUSD
	default:
		return rule.InputPerMillionTokensUSD + rule.OutputPerMillionTokensUSD
	}
}

func (c *Catalog) bestRule(provider, model string, rates rateSet) (*Rule, bool) {
	var best *compiledRule
	bestRate := 0.0
	for i := range c.rules {
		rule := &c.rules[i]
		if provider != "" && rule.rule.Provider != provider {
			continue
		}
		if !globMatch(rule.tokens, model) {
			continue
		}
		rate := rates.rate(&rule.rule)
		if rate <= 0 {
			continue
		}
		switch {
		case best == nil:
			best = rule
			bestRate = rate
		default:
			cmp := compareSpecificity(rule, best)
			if cmp > 0 || (cmp == 0 && rate > bestRate) {
				best = rule
				bestRate = rate
			}
		}
	}
	if best == nil {
		return nil, false
	}
	chosen := best.rule
	return &chosen, true
}

// estimateRule selects the rule that minimizes under-reservation when the
// provider is unknown: most specific pattern first, highest rate on ties.
func (c *Catalog) estimateRule(model string, rates rateSet, promptTokens, completionTokens, cacheRead, cacheWrite int) (*Rule, bool) {
	var best *compiledRule
	bestCost := -1.0
	for i := range c.rules {
		rule := &c.rules[i]
		if !globMatch(rule.tokens, model) {
			continue
		}
		rate := rates.rate(&rule.rule)
		if rate <= 0 {
			continue
		}
		var cost float64
		switch rates {
		case embeddingRates:
			cost = perMillion(promptTokens, rule.rule.EmbeddingPerMillionTokensUSD)
		case cachedRates:
			cost = cachedChatCost(&rule.rule, promptTokens, completionTokens, cacheRead, cacheWrite)
		default:
			cost = perMillion(promptTokens, rule.rule.InputPerMillionTokensUSD) + perMillion(completionTokens, rule.rule.OutputPerMillionTokensUSD)
		}
		switch {
		case best == nil:
			best, bestCost = rule, cost
		default:
			cmp := compareSpecificity(rule, best)
			if cmp > 0 || (cmp == 0 && cost > bestCost) {
				best, bestCost = rule, cost
			}
		}
	}
	if best == nil {
		return nil, false
	}
	chosen := best.rule
	return &chosen, true
}

func cachedChatCost(rule *Rule, uncached, completion, cacheRead, cacheWrite int) float64 {
	readRate := rule.CacheReadPerMillionTokensUSD
	if readRate == 0 {
		readRate = rule.InputPerMillionTokensUSD
	}
	writeRate := rule.CacheWritePerMillionTokensUSD
	if writeRate == 0 {
		writeRate = rule.InputPerMillionTokensUSD
	}
	return perMillion(uncached, rule.InputPerMillionTokensUSD) +
		perMillion(cacheRead, readRate) +
		perMillion(cacheWrite, writeRate) +
		perMillion(completion, rule.OutputPerMillionTokensUSD)
}

// compareSpecificity returns a positive value when a is a better pattern match
// than b for the same model: exact matches beat globs, then more literal
// characters, then a longer literal prefix (gpt-4o* beats gpt-*).
func compareSpecificity(a, b *compiledRule) int {
	switch {
	case a.score.exact != b.score.exact:
		if a.score.exact {
			return 1
		}
		return -1
	case a.score.literals != b.score.literals:
		if a.score.literals > b.score.literals {
			return 1
		}
		return -1
	case a.score.prefix != b.score.prefix:
		if a.score.prefix > b.score.prefix {
			return 1
		}
		return -1
	default:
		return 0
	}
}

func splitCacheTokens(promptTokens, cacheReadTokens, cacheWriteTokens int) (uncached, cacheRead, cacheWrite int) {
	promptTokens = clampNonNegative(promptTokens)
	cacheRead = clampNonNegative(cacheReadTokens)
	cacheWrite = clampNonNegative(cacheWriteTokens)
	if cacheRead > promptTokens {
		cacheRead = promptTokens
	}
	if cacheWrite > promptTokens-cacheRead {
		cacheWrite = promptTokens - cacheRead
	}
	return promptTokens - cacheRead - cacheWrite, cacheRead, cacheWrite
}

func clampNonNegative(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

func perMillion(tokens int, rate float64) float64 {
	return float64(tokens) * rate / tokensPerMillion
}
