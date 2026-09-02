package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

// cacheKeyPayload is canonical JSON for hashing.
type cacheKeyPayload struct {
	Model       string                `json:"model"`
	Messages    []provider.ChatMessage `json:"messages"`
	Temperature *float64              `json:"temperature,omitempty"`
	MaxTokens   *int                  `json:"max_tokens,omitempty"`
	Tools       []provider.Tool       `json:"tools,omitempty"`
}

// BuildKey creates SHA256 hex key from normalized request (model+messages+temperature+max_tokens).
func BuildKey(req provider.ChatRequest) string {
	payload := cacheKeyPayload{
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Tools:       req.Tools,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
