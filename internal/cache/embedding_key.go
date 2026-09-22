package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

type embeddingKeyPayload struct {
	Model          string `json:"model"`
	Input          any    `json:"input"`
	EncodingFormat string `json:"encoding_format,omitempty"`
	Dimensions     *int   `json:"dimensions,omitempty"`
	User           string `json:"user,omitempty"`
}

func BuildEmbeddingKey(req provider.EmbeddingRequest) string {
	payload := embeddingKeyPayload{
		Model:          req.Model,
		Input:          req.Input,
		EncodingFormat: req.EncodingFormat,
		Dimensions:     req.Dimensions,
		User:           req.User,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func BuildEmbeddingKeyForIdentity(req provider.EmbeddingRequest, identity string) string {
	base := BuildEmbeddingKey(req)
	if identity == "" {
		return base
	}
	sum := sha256.Sum256([]byte(identity + ":" + base))
	return hex.EncodeToString(sum[:])
}
