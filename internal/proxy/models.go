package proxy

import (
	"encoding/json"
	"net/http"
	"time"
)

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
	now := time.Now().Unix()
	var data []modelInfo
	for _, p := range h.registry.All() {
		for _, m := range p.Models() {
			data = append(data, modelInfo{
				ID:      m,
				Object:  "model",
				Created: now,
				OwnedBy: p.Name(),
			})
		}
	}
	if data == nil {
		data = []modelInfo{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelsResponse{
		Object: "list",
		Data:   data,
	})
}
