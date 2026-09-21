package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

// cacheKeyPayload is canonical JSON for hashing.
type cacheKeyPayload struct {
	Model       string                   `json:"model"`
	Messages    []provider.ChatMessage   `json:"messages"`
	Temperature *float64                 `json:"temperature,omitempty"`
	MaxTokens   *int                     `json:"max_tokens,omitempty"`
	Tools       []provider.Tool          `json:"tools,omitempty"`
	ToolChoice  any                      `json:"tool_choice,omitempty"`
	ResponseFmt *provider.ResponseFormat `json:"response_format,omitempty"`
	Stop        any                      `json:"stop,omitempty"`
	N           *int                     `json:"n,omitempty"`
	StreamOpts  *provider.StreamOptions  `json:"stream_options,omitempty"`
}

// BuildKey creates SHA256 hex key from normalized request (model+messages+temperature+max_tokens).
func BuildKey(req provider.ChatRequest) string {
	payload := cacheKeyPayload{
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
		ResponseFmt: req.ResponseFormat,
		Stop:        req.Stop,
		N:           req.N,
		StreamOpts:  req.StreamOptions,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// BuildKeyForIdentity isolates cached responses between authenticated clients.
// This prevents a shared cache from becoming an unintended cross-tenant data
// channel while keeping the request-only BuildKey useful for unit tests.
func BuildKeyForIdentity(req provider.ChatRequest, identity string) string {
	if identity == "" {
		return BuildKey(req)
	}
	base := BuildKey(req)
	sum := sha256.Sum256([]byte(identity + ":" + base))
	return hex.EncodeToString(sum[:])
}
