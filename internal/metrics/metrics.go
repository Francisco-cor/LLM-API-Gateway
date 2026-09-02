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

	CacheSize = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_cache_size",
		Help: "Current cache size (entries)",
	})
)

func ObserveTokens(provider string, prompt, completion int) {
	TokensTotal.WithLabelValues(provider, "prompt").Add(float64(prompt))
	TokensTotal.WithLabelValues(provider, "completion").Add(float64(completion))
	TokensTotal.WithLabelValues(provider, "total").Add(float64(prompt + completion))
}
