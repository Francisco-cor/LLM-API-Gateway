# Security Policy

## Supported versions

| Version | Supported | Notes |
|---|---|---|
| `1.1.x` | Yes | Current release line (`v1.1.0`). |
| `1.0.x` | Best effort | Fixes may be backported if the change is small and low-risk. |
| `< 1.0` | No | Pre-release history; upgrade. |

Only the latest patch of a supported minor receives security fixes. The `main` branch is development; a fix on `main` is not a released fix until it is tagged.

## Reporting a vulnerability

**Do not open a public issue for a security problem.**

Use GitHub's private vulnerability reporting:

1. Go to the repository's **Security** tab → **Advisories** → **Report a vulnerability**.
2. Or open a **private security advisory** directly from the repository page. GitHub keeps the discussion private until it is published.

Include, if possible:

- affected version or commit (`cat VERSION`, `git rev-parse HEAD`);
- a minimal reproduction (config snippet with secrets removed, request, observed vs expected);
- impact (confidentiality, integrity, availability, cost);
- whether the issue is reachable with a tenant API key, with admin access, or only with local access.

There is no security email address and no bug bounty. Do not send reports to personal addresses. If GitHub private reporting is unavailable, open a minimal public issue asking the maintainer to enable a private channel, without disclosing details.

**What to expect:** acknowledgement via the advisory, a triage decision, and coordination on disclosure. No fixed SLA is promised; this is a single-maintainer project. Please allow time for a fix before publishing. Credit is given in the advisory unless you ask otherwise.

## Scope

In scope:

- the gateway HTTP surface (`/v1/*`, `/health*`, `/metrics`);
- the admin API and pprof routes (`/admin/*`, `/debug/pprof/*`);
- authentication, authorization, tenant isolation, rate limiting, budget and cache logic;
- provider adapters and the translation layer, including secret handling and SSRF-relevant URL handling;
- the container image, Kubernetes manifests and default configuration in this repository;
- dependencies, when the vulnerability is exploitable through this project.

Out of scope:

- vulnerabilities in OpenAI, Anthropic, Gemini or any upstream provider;
- volumetric DoS, unless it exploits a specific unbounded resource in this code;
- model output quality, hallucinations, prompt injection that only affects the model's answer (report it if it crosses a trust boundary in the gateway);
- misconfiguration that is explicitly documented as insecure (for example `admin.allow_insecure: true` on a public interface);
- findings that require a compromised host or cluster-admin access.

## Threat model (summary)

### Assets

- Provider API keys (money and account access).
- Tenant API keys and their scopes.
- Prompt and completion content (potentially personal or confidential).
- Usage/budget state (financial integrity).
- Admin control plane (can rewrite configuration and expose pprof).

### Trust boundaries

```text
tenant client ──Bearer key──> gateway :8080 ──provider key──> upstream
                                   │
operator ──admin key──> admin :8081 ┘ (same process)
```

### Threats and current posture

| Threat | Current control | Gap / pending |
|---|---|---|
| **Admin API exposed** | Disabled-by-default auth is fail-closed: the process refuses to start when `admin.allow_insecure=false` and no key is set. Constant-time key comparison. Bind address defaults to `127.0.0.1`. | Admin key cannot be rotated without a restart; no per-operator identity or audit log; no mTLS. Roadmap R-09, R-26. |
| **pprof leaks secrets/prompts** | pprof is **opt-in** (`admin.pprof_enabled`, default false) and lives only in the admin mux; no `:6060` listener. | A heap dump still contains keys and prompts. Treat dumps as secrets, restrict admin network access, disable pprof after use. |
| **SSRF via `base_url`** | `base_url` is operator-controlled configuration, not tenant input; provider clients have a timeout and bounded response body (8 MiB). | No scheme/host allowlist, no private/loopback/link-local IP blocking, default HTTP redirects are followed, DNS rebinding is not addressed. Roadmap R-23. |
| **Tenant isolation** | Auth middleware derives the tenant from the key and **overwrites** `X-Tenant-ID`; cache keys are SHA-256-salted with the Authorization value; semantic namespaces include identity and response shape. Scopes and expiry are enforced per endpoint. Unit tests cover key identity isolation. | When `auth.enabled=false`, `X-Tenant-ID` is client-controlled (dev mode only). No end-to-end HTTP isolation suite across tenants; no tenant CRUD/revocation propagation (static config). Roadmap R-06/R-07. |
| **Secret exposure in logs** | `slog` redacts attributes named `authorization`, `x-api-key`, `api_key`, `api-key`, `key`, and string values that look like `Bearer ...` or `sk-...`; admin config responses redact provider/admin/Redis/auth keys as `***`. Gemini auth uses `x-goog-api-key`, not a query parameter. | Redaction is name/heuristic based; prompts/responses are not logged by default but can appear in errors. Provider error messages (bounded to 4 KiB) are returned to clients. No secret manager. Roadmap R-24/R-25. |
| **Prompt/content leakage via cache** | Cache entries are isolated by identity and response-shaping fields; TTL bounded; only 200 non-stream responses; no cache for streams. | Contents are stored unencrypted in memory/Redis; Redis is a separate trust boundary; semantic cache is local to a process. No PII classification. |
| **Credential stuffing / brute force** | API keys are high-entropy and compared by hash; missing keys use a constant-time dummy comparison. Rate limiting is per hashed identity. | No login endpoint (good), but no per-IP auth-failure alerting. |
| **Rate-limit bypass during Redis outage** | Local token bucket keeps per-process protection. | Global limit multiplies by replica count; the degraded flag has no metric. Roadmap R-27. |
| **Budget bypass / overspend** | Reservations are atomic (Redis Lua or in-process mutex); budget fails closed (`503`) when a configured Redis is unavailable; unknown prices can be rejected. | No ledger/reconciliation; no per-tenant quota UI; local mode is per replica. Roadmap R-10/R-11. |
| **Request smuggling / oversized bodies** | `MaxBytesReader` 1 MiB on chat/embeddings and admin PATCH; unknown JSON fields rejected; one JSON value per body; upstream bodies bounded. | No dedicated fuzzing of the translation layer in CI. |
| **Unauthenticated metrics** | `/metrics` is intentionally excluded from tenant auth. | It exposes traffic, cost, provider and tenant-adjacent labels. Restrict by network or add auth. Roadmap R-26. |
| **Supply chain** | `govulncheck` and `golangci-lint` (includes `gosec`) run in CI; base images pinned by tag, not digest. | No SBOM, no image signing, no dependency pinning by digest. |
| **Transport** | Gateway↔provider uses HTTPS by default; TLS verification is Go's default. | No mTLS to providers; Redis may be plaintext in the compose profile; no TLS termination in the repo. |

### Explicit non-goals today

- The gateway is **not** a compliance boundary (HIPAA, PCI, SOC 2). It does not classify or redact PII.
- It does not defend against a malicious operator with admin access: the admin API can change routes and provider URLs by design.
- It does not sandbox model output or tool execution; the client executes tools.

## Hardening checklist for operators

- [ ] Set `ADMIN_API_KEY` to a random value ≥ 32 bytes; never use `admin.allow_insecure: true` outside localhost.
- [ ] Keep `admin.bind_address: 127.0.0.1` unless you have a NetworkPolicy/ACL; do not expose `:8081` through a public Ingress.
- [ ] Leave `admin.pprof_enabled: false`; enable it only during an incident, download dumps securely, delete them, disable it.
- [ ] Require auth (`auth.enabled: true`) for every non-local deployment; scope keys to `chat:write`, `embeddings:write`, `models:read`.
- [ ] Put Redis on a private network with auth/TLS; do not reuse it for unrelated data.
- [ ] Restrict `/metrics` to the monitoring network or add an authenticating proxy.
- [ ] Set `rate_limit.budget.enabled: true` and a real monthly limit; monitor `gateway_cost_usd_total`.
- [ ] Use `readOnlyRootFilesystem`, `runAsNonRoot`, dropped capabilities (the K8s manifests do this).
- [ ] Pin image digests and scan them (`docker scout`, `trivy`) in your own pipeline.
- [ ] Rotate tenant keys with overlap; remember the admin key requires a restart.
- [ ] Back up `config.yaml`/secrets and test restore; until Postgres exists, the YAML is the control plane.

## Current controls vs pending (quick view)

| Control | Status |
|---|---|
| Tenant Bearer auth, scopes, expiry | Implemented |
| Admin fail-closed + constant-time compare | Implemented |
| Admin bind loopback by default | Implemented |
| pprof opt-in inside admin only | Implemented |
| Secret redaction in logs/admin output | Implemented (heuristic) |
| Request/response size bounds | Implemented |
| CORS allowlist, security headers | Implemented |
| Budget fail-closed with Redis | Implemented |
| SSRF allowlist / private-IP blocking | **Pending** (R-23) |
| Secret manager + rotation | **Pending** (R-24) |
| PII redaction / prompt logging policy | **Pending** (R-25) |
| Admin audit log + operator identity | **Pending** (R-09) |
| Metrics authentication | **Pending** (R-26) |
| SBOM + image signing | **Pending** |
| Ledger/reconciliation | **Pending** (R-10/R-12) |

If you are evaluating this project for production, treat the pending items as blockers, not as nice-to-haves.
