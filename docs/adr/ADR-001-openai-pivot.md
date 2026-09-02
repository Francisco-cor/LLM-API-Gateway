# ADR-001: OpenAI as Pivot Format

**Status:** Accepted · **Date:** 2026-09-01 · **Deciders:** Gateway team
**Context:** Need to unify OpenAI, Anthropic, Gemini behind single API.

## Context
Clients already speak OpenAI JSON (`messages`/`choices`/`usage`). Need 1 canonical shape to avoid N×N translation.

## Decision
Use OpenAI `ChatRequest/ChatResponse` as pivot (`internal/types/types.go`, `internal/provider/provider.go:8` re-exports).
Providers implement `Send(ctx, ChatRequest) (ChatResponse, error)` + `SendStream`. Translation lives in `internal/translate/` (`ToAnthropic`, `FromAnthropic`, `ToGemini`, `FromGemini`).

- Anthropic: `system` extracted, `assistant→model` role map, `anthropic-version:2023-06-01`, `MaxTokens` defaults 1024, streaming `event:content_block_delta/message_stop` → `StreamChunk`.
- Gemini: `messages→contents` (`user`/`model`), `generateContent?key=` + `streamGenerateContent?alt=sse`, `candidates[0].content.parts[0].text` → `choices[0].message`.

## Alternatives considered
- **Anthropic pivot:** rejected — Gemini `contents` shape farther, less SDK adoption.
- **Internal IR:** rejected — extra mapping layer; OpenAI already 90% mapping clean.
- **No pivot (native pass-through):** rejected — client would need per-provider branches.

## Consequences
+ SDK compat: `openai-python/node/go` work unchanged (`base_url` swap).
+ Tests: `tests/translate_test.go` 85%+ cover, isolated.
+ Provider add = 1 file + translate funcs.
- OpenAI quirks leak: `Tool` shape must be translated for Gemini `functionDeclarations` (future).
- `Gemini API key` in query `?key=` is log-sensitive → redacted in logger (`fix(security)`).

## Links
- `internal/translate/anthropic.go`, `gemini.go`, `openai.go`
- `tests/translate_test.go`, `tests/embeddings_test.go`
