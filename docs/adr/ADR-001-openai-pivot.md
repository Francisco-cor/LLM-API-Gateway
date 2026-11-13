# ADR-001: OpenAI as Pivot Format

**Status:** Accepted · **Date:** 2026-09-01 · **Deciders:** Gateway team
**Context:** Need to unify OpenAI, Anthropic, Gemini behind single API.

## Context
Clients already speak OpenAI JSON (`messages`/`choices`/`usage`). Need 1 canonical shape to avoid N×N translation.

## Decision
Use OpenAI `ChatRequest/ChatResponse` as pivot (`internal/types/types.go`; `internal/provider/provider.go` re-exports type aliases).
Providers implement `Send(ctx, ChatRequest) (ChatResponse, error)` + `SendStream`. Translation lives in `internal/translate/` (`ToAnthropic`, `FromAnthropic`, `ToGemini`, `FromGemini`).

- Anthropic: `system` extracted, messages mapped to Anthropic roles, `anthropic-version: 2023-06-01`, `MaxTokens` defaults **4096**, streaming `event:content_block_delta/message_stop` → `StreamChunk`.
- Gemini: `messages→contents` (`user`/`model`), `generateContent` + `streamGenerateContent?alt=sse`, `x-goog-api-key` header (not a query parameter), `candidates[0].content.parts[0].text` → `choices[0].message`.

## Alternatives considered
- **Anthropic pivot:** rejected — Gemini `contents` shape farther, less SDK adoption.
- **Internal IR:** rejected — extra mapping layer; OpenAI already 90% mapping clean.
- **No pivot (native pass-through):** rejected — client would need per-provider branches.

## Consequences
+ SDK compat: `openai-python/node` work unchanged for the supported surface (`base_url` swap); conformance against official SDKs in CI is still pending.
+ Tests: translation is covered by `tests/translate_test.go` with `httptest` fixtures; no coverage percentage is claimed.
+ Provider add = 1 adapter + translate functions (today it also requires a branch in `buildProviders`; a registry is roadmap R-18).
- OpenAI tool definitions and calls are translated to Gemini `functionDeclarations/functionCall` and Anthropic `tool_use/tool_result`; provider-specific capabilities still require contract fixtures.
- Gemini authentication uses the `x-goog-api-key` header, so the key is not exposed in URLs. Log redaction still applies to headers and `api_key` fields.

## Links
- `internal/translate/anthropic.go`, `gemini.go`, `openai.go`
- `tests/translate_test.go`, `tests/embeddings_test.go`
