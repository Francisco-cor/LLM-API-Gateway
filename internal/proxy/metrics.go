package proxy

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewMetricsHandler returns a handler that exposes Prometheus metrics.
// It replaces the JSON placeholder from Fase 3.
func NewMetricsHandler() http.Handler {
	return promhttp.Handler()
}
