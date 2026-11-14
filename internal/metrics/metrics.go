package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_requests_total",
		Help: "Total HTTP requests handled by the gateway",
	}, []string{"method", "path", "status", "provider"})

	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_request_duration_seconds",
		Help:    "Histogram of request latency",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path", "provider"})

	TokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_tokens_total",
		Help: "Total tokens processed (prompt + completion)",
	}, []string{"provider", "type"}) // type=prompt|completion|total

	CostsUSDTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_cost_usd_total",
		Help: "Estimated provider cost in USD, split by pricing provenance",
	}, []string{"provider", "model", "pricing"}) // pricing=known|fallback

	ProviderErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_provider_errors_total",
		Help: "Provider errors by provider and code",
	}, []string{"provider", "code"})

	CircuitState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_circuit_state",
		Help: "Circuit breaker state (0=closed,1=open,2=half-open)",
	}, []string{"provider"})

	CacheHits = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_cache_hits_total",
		Help: "Cache hits vs misses",
	}, []string{"result"}) // result=hit|miss

	// CapabilityMismatches counts requests rejected before any provider call
	// and fallbacks skipped because a model cannot serve tools/vision/JSON.
	CapabilityMismatches = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_capability_mismatches_total",
		Help: "Capability mismatches by provider, missing capability and action",
	}, []string{"provider", "capability", "action"}) // action=request_rejected|fallback_skipped

	// IdempotencyRequests counts Idempotency-Key outcomes.
	IdempotencyRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_idempotency_requests_total",
		Help: "Idempotency-Key outcomes",
	}, []string{"result"}) // result=miss|replay|conflict|in_progress|invalid|bypass

	CacheSize = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_cache_size",
		Help: "Current cache size (entries)",
	})

	// StreamTTFT measures time from accepting the request to the first byte of
	// the first SSE chunk. Streaming latency is dominated by this number, which
	// request_duration conflates with the whole completion.
	StreamTTFT = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_stream_ttft_seconds",
		Help:    "Time to first streamed token (first SSE chunk)",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
	}, []string{"provider"})

	// StreamDuration measures how long the SSE response stayed open. A stream
	// cut short by an upstream failure is recorded too, so the histogram is
	// also a coarse view of incomplete generations.
	StreamDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_stream_duration_seconds",
		Help:    "Duration of streamed responses",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
	}, []string{"provider"})
)

func ObserveTokens(provider string, prompt, completion int) {
	TokensTotal.WithLabelValues(provider, "prompt").Add(float64(prompt))
	TokensTotal.WithLabelValues(provider, "completion").Add(float64(completion))
	TokensTotal.WithLabelValues(provider, "total").Add(float64(prompt + completion))
}

func ObserveCost(provider, model string, usd float64, known bool) {
	if usd <= 0 {
		return
	}
	pricingSource := "fallback"
	if known {
		pricingSource = "known"
	}
	CostsUSDTotal.WithLabelValues(provider, model, pricingSource).Add(usd)
}
