package translate

import "github.com/fcordero/llm-api-gateway/internal/types"

// OpenAI types are already the pivot format, so no translation needed.
// This file exists for symmetry and future validation/normalization.

// NormalizeChatRequest performs light normalization on an OpenAI request
// (e.g., defaulting N, ensuring messages non-empty). Currently passthrough
// but provides a hook for future validation.
func NormalizeChatRequest(req types.ChatRequest) types.ChatRequest {
	return req
}
