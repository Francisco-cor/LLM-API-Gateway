package proxy

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// MetricsHandler exposes basic gateway metrics without external deps.
// Full Prometheus support lands in Fase 5; this placeholder keeps build green
// and provides a useful debug endpoint.
type MetricsHandler struct {
	startTime time.Time
	requests  atomic.Int64
}

func NewMetricsHandler() *MetricsHandler {
	return &MetricsHandler{startTime: time.Now()}
}

func (h *MetricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.requests.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"uptime_seconds": time.Since(h.startTime).Seconds(),
		"requests":       h.requests.Load(),
		"note":           "prometheus metrics coming in Fase 5",
	})
}
