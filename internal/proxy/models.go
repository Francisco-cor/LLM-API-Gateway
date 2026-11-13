package proxy

import (
	"encoding/json"
	"net/http"
)

// modelCreatedAt is a fixed, stable release timestamp for the model listing.
// time.Now() made every /v1/models response claim a new creation time, which
// breaks clients that use `created` for ordering or cache validation.
const modelCreatedAt int64 = 1704067200 // 2024-01-01T00:00:00Z

// ModelsHandler serves GET /v1/models (OpenAI-compatible).
type ModelsHandler struct {
	registry *Registry
}

func NewModelsHandler(registry *Registry) *ModelsHandler {
	return &ModelsHandler{registry: registry}
}

type modelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type modelsResponse struct {
	Object string      `json:"object"`
	Data   []modelInfo `json:"data"`
}

func (h *ModelsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Patterns are routing implementation details ("gpt-4*" is not a model
	// callers can send). Publish concrete models only, and deduplicate IDs that
	// several providers or exact entries claim.
	seen := make(map[string]bool)
	data := make([]modelInfo, 0)
	for _, p := range h.registry.All() {
		for _, m := range p.Models() {
			if isPattern(m) || seen[m] {
				continue
			}
			seen[m] = true
			data = append(data, modelInfo{
				ID:      m,
				Object:  "model",
				Created: modelCreatedAt,
				OwnedBy: p.Name(),
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelsResponse{
		Object: "list",
		Data:   data,
	})
}
