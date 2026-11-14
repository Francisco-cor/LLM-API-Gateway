# ROADMAP — de proyecto de entrevista a producto operable

> **Estado:** documento vivo · **Última revisión:** 2026-11-13 · **Rama:** `docs/final` (integrada en `main`)
> **Alcance:** qué es hoy el gateway, qué le falta para operar como producto y en qué orden hacerlo.
> Este documento es la fuente de verdad del plan. El README resume y enlaza aquí; no dupliques el backlog en otros sitios.

---

## 1. Veredicto honesto

El repositorio es un **data plane de gateway LLM muy por encima de la media de un proyecto de entrevista**. La parte difícil —traducir tres APIs a un contrato OpenAI, streaming SSE, fallback con reintentos, circuit breaker, rate limit token-aware, budget, caché exacta y semántica, recarga en caliente y observabilidad— está implementada y cubierta por tests de contrato y de integración con `httptest`.

No es todavía un **producto SaaS multi-tenant**. Le faltan las cosas que no se ven en una demo:

- no hay **control plane persistente**: la configuración vive en YAML y en memoria del proceso; no hay Postgres, migraciones, CRUD de tenants/keys ni historial de cambios;
- no hay **ledger de uso**: el budget se calcula con contadores mensuales en memoria/Redis, sin eventos auditables ni reconciliación;
- el routing ya conoce capacidades por modelo (tools/visión/audio/JSON/embeddings) y las respeta en fallback, pero no hay routing por coste, latencia ni tenant;
- el soporte multi-proveedor es **tres adaptadores hardcodeados** (`openai`, `anthropic`, `gemini`) en `cmd/gateway/main.go`, no un registro genérico;
- no hay **límites de concurrencia/backpressure**, **SSRF guard** para `base_url`, **secret manager**, **PII**, **Helm/Terraform**, **UI**, **conformance con SDKs oficiales** ni **SLOs medidos**.

**Para enseñar en una entrevista:** excelente. Enseña decisiones reales de ingeniería y sus trade-offs.
**Para vender como producto:** beta incompleto. Lo que hay que arreglar primero no es más código de proxy; es control plane, persistencia, cuotas y confianza operativa.

### Lo que un evaluador puede verificar en 15 minutos

| Señal | Dónde mirar | Comentario |
|---|---|---|
| Contrato OpenAI real | `internal/types/types.go`, `tests/contract/openai_compat_test.go` | Chat, tools, streaming, embeddings, errores. |
| Traducción a Anthropic/Gemini | `internal/translate/`, `internal/provider/` | Herramientas nativas preservadas; proyección de texto para lo no soportado. |
| Resiliencia | `internal/resilience/`, `internal/proxy/handler.go` | Retry con jitter, circuit breaker, hedge solo en no-stream. |
| Límites y coste | `internal/ratelimit/`, `internal/budget/`, `internal/pricing/` | Buckets por shard, Redis Lua, reserva/commit atómica. |
| Operación | `internal/admin/server.go`, `deploy/` | Admin fail-closed, pprof opt-in, recarga validada. |
| Honestidad | este documento, `BENCH.md`, `SECURITY.md` | Los números no medidos están marcados como tales. |

---

## 2. Matriz de madurez

Niveles: **prototipo** (funciona en local, sin garantías), **beta** (usable con supervisión, faltan garantías de producto), **profesional** (contrato, persistencia, SLOs, seguridad y soporte).

| Dimensión | Nivel | Evidencia verificable | Qué falta para profesional |
|---|---|---|---|
| Contrato HTTP OpenAI | **beta** | `POST /v1/chat/completions`, `/v1/embeddings`, `GET /v1/models`; contrato tolerante a campos desconocidos; `api/openapi.yaml` 1.1.1 con `param`, `index`, usage details, `json_schema` e idempotencia | Conformance contra SDKs oficiales en CI; versionado de contrato; `/v1/responses`, audio, batches fuera de alcance |
| Proveedores | **prototipo** | 3 adaptadores en `buildProviders`; interfaz `Provider` + `Embedder`; salud por proveedor | Registro genérico OpenAI-compatible, Azure/Bedrock/Vertex, presets (Groq, DeepSeek, OpenRouter, Ollama, vLLM), fixtures de contrato por proveedor |
| Routing | **beta** | exacto, glob/regex, `routing.weighted`, aliases, auto-discovery, capability registry con fallback restringido | Routing por coste, latencia y tenant; canary por cabecera; presupuesto de contexto |
| Resiliencia | **beta** | retry 3× con jitter, circuit 5→30 s, hedge 300 ms (no-stream), fallback antes del primer chunk SSE, streaming truncado honesto | Backpressure, bulkheads, timeouts por ruta, presupuesto de reintentos global, hedge en streaming |
| Control plane | **prototipo** | YAML + `SIGHUP` + watcher con reintento + admin `:8081`; validación y rollback; ETag/If-Match, audit y rotación de admin key en caliente | Postgres, migraciones, CRUD tenants/keys/proveedores/rutas, versionado y auditoría persistente, coherencia entre réplicas |
| Multi-tenancy | **beta** | API keys con scopes obligatorios/allowlist, expiración, pepper y throttle 401; `X-Tenant-ID` derivado de la key | CRUD, rotación con solape entre réplicas, cuotas por tenant/modelo, aislamiento verificable, límites de modelo por tenant |
| Uso y coste | **prototipo** | budget mensual UTC con reservas-lease y reconciliación; catálogo por millón; cached tokens a tarifa de caché; `gateway_cost_usd_total` | Ledger append-only, reconciliación contra ledger, cuotas, facturación, exportación, precio efectivo por fecha |
| Caché | **beta** | exacta en memoria/Redis, `max_bytes`/`max_ttl`, single-flight, namespace por identidad y forma, semántica opt-in con un solo embedding, stats Redis reales | Índice semántico compartido entre réplicas, invalidación, TTL por tenant, métricas de ahorro |
| Observabilidad | **beta** | Prometheus (10 métricas, incl. TTFT/duración de stream), OTLP/gRPC, `slog` con `request_id`; `/metrics` | SLOs, dashboards, alertas como código, latencia de upstream separada, métricas de budget/cola/Redis, auth de `/metrics` |
| Seguridad | **beta** | auth Bearer con scopes fail-closed, admin fail-closed/ETag/audit/rotación, pprof opt-in, bind loopback, pepper, throttle 401, strict mode y validación de `base_url` (https, metadata/privadas), redacción de userinfo | SSRF con DNS/redirect, secret manager, PII, mTLS, identidad de operador, `/metrics` auth |
| Despliegue | **beta** | Dockerfile multi-stage, manifiestos K8s (Deployment/HPA/PDB/ServiceMonitor/NetworkPolicy/SA/topology), SBOM+cosign, perfil prod de Compose | Helm, Terraform, migraciones, e2e en kind, rollback probado |
| Calidad | **beta** | tests unitarios/contrato, `-race` en CI, gate de cobertura 55 %, govulncheck, golangci-lint v2, job de integración Redis opt-in | e2e con upstream falso, conformance SDK, pruebas de carga versionadas, caos de Redis/proveedor |
| Rendimiento | **prototipo** | microbenchmarks medidos (2026-11-13, hardware/comando publicados); sin load test e2e | Arnés de carga con upstream falso, presupuesto de latencia y resultados publicados |
| Documentación | **beta** | README, arquitectura, ADRs 001–005, runbook, seguridad, OpenAPI 1.1.1, roadmap | Guías de migración, troubleshooting probado, doc de API generada desde el contrato |

**Lectura:** el data plane está en beta alto; el control plane y el negocio (tenancy, uso, cuotas) están en prototipo. No tiene sentido añadir proveedores antes de resolver persistencia y cuotas: se multiplicaría la superficie sin poder cobrar ni auditar.

---

## 3. Decisiones rectoras

1. **Data plane sin base de datos en el hot path.** El gateway resuelve, limita, enruta y proxya con estado en memoria + Redis. Postgres es del control plane. El ledger se escribe de forma asíncrona por lotes; si la cola se llena, se aplica la política definida (ver ADR-004), no se bloquea la respuesta al cliente.
2. **Fail-closed donde hay dinero o control.** Admin sin key no arranca; budget sin backend fiable devuelve 503; cuotas no se degradan a “ilimitado”. Caché y rate limit distribuido tienen políticas explícitas y alertables, no fallbacks silenciosos.
3. **Contrato primero.** `api/openapi.yaml` es la referencia pública. Un cambio incompatible exige nueva versión de contrato y test de conformidad.
4. **Capacidades, no nombres de modelo.** El routing debe razonar sobre `tools`, `vision`, `json_schema`, `streaming`, `context_window` y `embeddings`, no sobre prefijos hardcodeados.
5. **Aburrido y observable.** Postgres, Redis, Prometheus, OTLP, Helm. Sin operador de Kubernetes ni WASM hasta que el producto lo pida.
6. **Nada se declara medido si no se puede reproducir.** Hardware, fecha y comando junto a cada cifra; si no, se etiqueta como referencia local.

Estas decisiones se desarrollan en [`ADR-004-professionalization.md`](adr/ADR-004-professionalization.md).

---

## 4. Fases

Esfuerzo orientativo para una persona con dedicación parcial: **S** ≤ 3 días, **M** ≤ 2 semanas, **L** > 2 semanas. “Entrevista” significa que ya aporta una historia técnica defendible; “producto” significa que cierra una brecha de adopción real.

### Fase 0 — Verdad documental y guardrails (S, entrevista + producto)

**Por qué:** una demo pierde credibilidad en cuanto un número o un endpoint no coincide con el código.

**Entregables:** README reescrito y verificado; BENCH sin benchmarks inexistentes; CHANGELOG honesto; OpenAPI 3.1 validado; runbook y SECURITY; plantillas de contribución.

**Aceptación medible:**
- `python3 -c "import yaml; yaml.safe_load(open('api/openapi.yaml'))"` termina con código 0.
- Ningún enlace relativo de la documentación apunta a un archivo inexistente (script de comprobación en CI, pendiente de workflow).
- README no afirma cobertura, tamaño de imagen, número de dependencias ni “production-ready” sin evidencia reproducible.

**Riesgo:** se corrige la documentación pero no el producto; por eso la fase 1 no es opcional.

### Fase 1 — Contrato y conformidad (M, producto)

**Por qué:** “OpenAI-compatible” sin pruebas contra el SDK oficial es una promesa, no una garantía.

**Entregables:**
- OpenAPI como fuente de verdad y chequeo de cambios incompatibles.
- Suite de conformidad que arranque el gateway contra un upstream falso y ejecute `openai-python` y `openai-node` reales (chat, stream, tools, embeddings, errores).
- Idempotency-Key en `POST /v1/chat/completions` y `/v1/embeddings` (Redis/Postgres, TTL 24 h, misma respuesta para la misma clave y cuerpo).
- Tests de campos desconocidos (tolerados en data plane), cuerpos >8 MiB (413), JSON trailing y `stream_options.include_usage`.

**Aceptación medible:** 100 % de los casos de la matriz de conformidad pasan en CI; una petición repetida con la misma `Idempotency-Key` no genera doble cargo en el ledger (cuando exista, fase 3).

### Fase 2 — Control plane persistente (L, producto)

**Por qué:** sin CRUD ni persistencia no hay producto multi-tenant; cada cambio exige editar YAML y recargar procesos.

**Entregables:**
- Postgres 15+ con migraciones versionadas (herramienta única: goose o atlas; decidir en el PR).
- Tablas `tenants`, `api_keys`, `providers`, `models`, `prices`, `routes`, `budgets`, `usage_events`, `audit_log` (esquema en §6).
- Admin API para crear/rotar/revocar tenants y keys, y para publicar rutas/proveedores. La config YAML queda como bootstrap/importación, no como estado autoritativo.
- Sincronización de estado a réplicas: polling con ETag/versión o LISTEN/NOTIFY; objetivo de propagación ≤ 5 s.
- Auditoría de cada cambio con actor, antes/después e IP.

**Aceptación medible:**
- Crear tenant + key por API y usarla contra `/v1/chat/completions` sin reiniciar.
- Revocar una key: 401 en todas las réplicas en ≤ 5 s.
- Reiniciar el proceso: tenants, keys y rutas persisten.
- Migración desde el YAML actual documentada y probada en una base vacía.

### Fase 3 — Ledger de uso y cuotas (L, producto)

**Por qué:** el budget actual es un contador; no permite auditar, reconciliar ni cobrar.

**Entregables:**
- `usage_events` append-only con `request_id`, tenant, key, proveedor, modelo, tokens, coste, estado y latencia.
- Escritura asíncrona por lotes desde el data plane (buffer acotado + backpressure; ver ADR-004).
- Cuotas por tenant y por modelo (tokens y USD) calculadas contra el ledger/rollup, no solo contra un contador mensual.
- Reconciliación diaria: suma del ledger vs contadores de budget; alerta si el desvío > 0,5 %.
- Exportación CSV/parquet o endpoint de consulta para finanzas.

**Aceptación medible:** desvío de reconciliación < 0,5 % durante 7 días; una cuota agotada devuelve 429 `insufficient_quota` y no llega al proveedor; el ledger conserva el 100 % de las peticiones aceptadas (medido con muestreo de `request_id`).

### Fase 4 — Capability registry y fallback por capacidad (M, producto)

**Por qué:** fallback por nombre de modelo funciona en demo; en producción un modelo sin tools o sin visión rompe al cliente.

**Entregables:**
- Metadatos por modelo: `chat`, `stream`, `tools`, `parallel_tools`, `vision`, `audio`, `json_schema`, `embeddings`, `context_window`, `max_output`.
- Validación temprana: si la petición usa tools/visión y el modelo resuelto no lo soporta, 400 accionable antes de gastar una llamada.
- Fallback solo a modelos que conserven las capacidades requeridas; si no hay candidato, error explícito (no degradación silenciosa).
- Tests de matriz capacidades × proveedor.

**Aceptación medible:** 0 peticiones con `tools` enrutadas a un modelo sin tools en la suite de conformidad; todo fallback registra la capacidad que lo motivó.

### Fase 5 — Amplitud de proveedores (L, producto)

**Por qué:** tres proveedores no justifican un gateway; el valor está en cambiar de proveedor sin cambiar de SDK.

**Entregables:**
- Adaptador **OpenAI-compatible genérico**: `base_url`, `api_key`, cabecera de auth configurable, path de chat/embeddings/models, timeouts. Presets para Groq, DeepSeek, OpenRouter, Ollama y vLLM.
- Azure OpenAI (deployment name, `api-version`), AWS Bedrock (SigV4, modelos con prefijo de proveedor), Google Vertex (OAuth/service account).
- Fixtures de contrato por proveedor y un test que garantice que un adaptador nuevo no rompe el contrato común.
- Documentar qué campos OpenAI se ignoran en cada adaptador (p. ej. `logprobs`, `n`, `seed`).

**Aceptación medible:** añadir un proveedor OpenAI-compatible requiere solo configuración, sin tocar `cmd/gateway/main.go`; la suite de contrato pasa para al menos 3 backends simulados.

### Fase 6 — Seguridad de producto (M, producto)

**Por qué:** el admin puede reescribir configuración y el gateway guarda claves de proveedor; el riesgo no es teórico.

**Entregables:**
- **SSRF:** validar `base_url` (https por defecto, allowlist de hosts, bloqueo de IP privadas/loopback/link-local, no seguir redirects a hosts no permitidos, timeout de DNS).
- **Secretos:** interfaz `SecretProvider` (env, archivo, Vault/AWS Secrets Manager); nunca secretos en el YAML versionado; rotación sin reinicio cuando el backend lo permita.
- **PII:** hooks de redacción antes de logs y de caché; por defecto no registrar prompts ni respuestas.
- **Admin:** auditoría de accesos (implementada como log `admin_audit`, no persistente), mTLS o NetworkPolicy (NetworkPolicy ya existe), rotación de `ADMIN_API_KEY` sin reinicio (hecha), `/metrics` autenticado o en un listener interno (pendiente).
- **Multi-tenant:** pruebas de que un tenant no puede leer caché, contadores ni budget de otro; límites de modelo y de tokens por tenant.

**Aceptación medible:** suite de abuso (SSRF a `127.0.0.1`, `169.254.169.254`, redirect a interno) bloqueada; grep de logs de una suite de 1 000 peticiones no contiene claves ni prompts; rotación de admin key probada en caliente.

### Fase 7 — SLOs y backpressure (M, producto)

**Por qué:** sin límites de concurrencia, una subida de latencia del proveedor se convierte en OOM o en timeouts en cascada.

**Entregables:**
- SLOs publicados (disponibilidad, error rate, latencia de overhead, frescura de configuración).
- Límite de concurrencia por proveedor/ruta, cola acotada y shedding con 429/503 + `Retry-After`.
- Timeouts diferenciados: cliente, gateway, proveedor; presupuesto de reintentos.
- Alertas como código (Prometheus rules) y dashboards versionados.

**Aceptación medible:** con el upstream al 200 % de latencia nominal, el gateway mantiene memoria y goroutines acotadas y devuelve 503 con `Retry-After` en lugar de agotar el proceso; se publica el resultado del ensayo con hardware, fecha y comando.

### Fase 8 — Empaquetado y entrega (M, producto)

**Por qué:** los manifiestos K8s actuales son un ejemplo, no un producto instalable.

**Entregables:** Helm chart (values, probes, HPA, PDB, ServiceMonitor, NetworkPolicy), módulo Terraform para el despliegue gestionado, e2e en kind con upstream falso, guía de upgrade/rollback y migraciones.

**Aceptación medible:** `helm install` en un cluster kind desde cero sirve `/health` y `/v1/models`; `helm upgrade` y `helm rollback` conservan el servicio; `terraform plan` es reproducible y no contiene secretos.

### Fase 9 — UI mínima (M, opcional/producto)

**Por qué:** operar tenants y ver consumo por `curl` no escala a un equipo.

**Entregables:** login con el mismo admin auth, lista/creación/revocación de keys, uso y coste por tenant, salud de proveedores, editor de rutas con validación. Sin diseño de producto ambicioso: una consola interna.

**Aceptación medible:** un operador sin acceso al YAML crea un tenant, emite una key y ve su consumo del día.

---

## 5. Backlog priorizado (30 items)

Prioridad: **P0** bloquea adopción; **P1** necesaria para operar; **P2** mejora competitiva. “Entrevista” = ya demuestra criterio; “Producto” = cierra una brecha real.

| ID | Item | Fase | Prioridad | Impacto | Esfuerzo | Valor |
|---|---|---|---|---|---|---|
| R-01 | OpenAPI validado en CI y detección de cambios incompatibles | 0–1 | P0 | Alto | S | Entrevista + Producto |
| R-02 | Conformance con SDKs oficiales (Python/Node) contra upstream falso | 1 | P0 | Alto | M | Entrevista + Producto |
| R-03 | Idempotency-Key en chat y embeddings | 1 | P0 | Alto | M | Producto |
| R-04 | Gate de cobertura y e2e con upstream falso en CI | 0–1 | P0 | Medio | S | Entrevista |
| R-05 | Postgres + migraciones versionadas | 2 | P0 | Muy alto | L | Producto |
| R-06 | CRUD de tenants y API keys por admin API | 2 | P0 | Muy alto | M | Producto |
| R-07 | Rotación de keys con solape y revocación ≤ 5 s | 2 | P0 | Alto | M | Producto |
| R-08 | Propagación de configuración entre réplicas (versión/ETag/notify) | 2 | P0 | Alto | M | Producto |
| R-09 | Auditoría de cambios de control plane | 2 | P1 | Alto | S | Producto |
| R-10 | Ledger de uso asíncrono y append-only | 3 | P0 | Muy alto | L | Producto |
| R-11 | Cuotas por tenant y modelo contra ledger | 3 | P0 | Muy alto | M | Producto |
| R-12 | Reconciliación diaria y alerta de desvío | 3 | P1 | Alto | S | Producto |
| R-13 | Catálogo de precios versionado por fecha efectiva | 3 | P1 | Alto | S | Producto |
| R-14 | Métrica de budget restante y burn rate por tenant | 3 | P1 | Alto | S | Producto |
| R-15 | Capability registry por modelo | 4 | P0 | Alto | M | Producto |
| R-16 | Fallback restringido por capacidades requeridas | 4 | P0 | Alto | M | Producto |
| R-17 | Validación temprana de tools/visión/JSON schema | 4 | P1 | Medio | S | Producto |
| R-18 | Adaptador OpenAI-compatible genérico | 5 | P0 | Muy alto | M | Entrevista + Producto |
| R-19 | Presets Groq, DeepSeek, OpenRouter, Ollama, vLLM | 5 | P1 | Alto | S | Producto |
| R-20 | Azure OpenAI (deployment/api-version) | 5 | P1 | Alto | M | Producto |
| R-21 | AWS Bedrock (SigV4) y Google Vertex (OAuth) | 5 | P2 | Medio | L | Producto |
| R-22 | Fixtures de contrato por proveedor | 5 | P1 | Alto | M | Producto |
| R-23 | SSRF guard para `base_url` (allowlist, IP privadas, redirects) | 6 | P0 | Alto | M | Producto |
| R-24 | Secret manager y rotación sin secretos en YAML | 6 | P0 | Alto | M | Producto |
| R-25 | Redacción de PII y prompts fuera de logs por defecto | 6 | P1 | Alto | M | Producto |
| R-26 | `/metrics` autenticado y admin key rotable sin reinicio | 6 | P1 | Medio | S | Producto |
| R-27 | Límite de concurrencia, cola y shedding con Retry-After | 7 | P0 | Alto | M | Producto |
| R-28 | SLOs, dashboards y alertas como código | 7 | P1 | Alto | M | Producto |
| R-29 | Helm + Terraform + e2e kind | 8 | P1 | Alto | M | Producto |
| R-30 | UI mínima de tenants/keys/consumo | 9 | P2 | Medio | M | Producto |

**Estado del backlog a 2026-11-13.** Solo se marcan como cerrados o parciales los items tocados por esta sesión; el resto sigue pendiente.

| ID | Estado | Evidencia / qué falta |
|---|---|---|
| R-01 | **Cerrado** | `redocly lint --max-problems 0` (OpenAPI 3.1, zero warnings) y `oasdiff breaking --fail-on ERR` contra la rama base del PR o `HEAD^` en push directo, en el job `openapi` de CI. Los cambios incompatibles intencionados requieren el marcador `[openapi-breaking-approved]`. Local: `make openapi-lint` / `make openapi-breaking`. |
| R-03 | **Cerrado** | `internal/idempotency`: `Idempotency-Key` por tenant+ruta, fingerprint del body, replay de 2xx con `Idempotent-Replayed`, 409 `idempotency_key_reuse`/`in_progress`, single-flight local + lock Redis `SET NX`; store LRU acotado + Redis. Tests unitarios, de middleware y de integración cross-réplica. |
| R-04 | **Parcial** | Gate de cobertura al 55 % con `-coverpkg=./...` y job de integración Redis opt-in; falta el e2e con upstream falso. |
| R-07 | **Parcial** | Keys de tenant y admin key se recargan en caliente; el solape entre réplicas y la revocación ≤ 5 s requieren control plane. |
| R-09 | **Parcial** | Audit `admin_audit` con actor fingerprint, acción y detalle; es log de proceso, no tabla persistente ni identidad de operador. |
| R-13 | **Parcial** | Catálogo versionado y tarifas por millón con cache read/write; falta precio efectivo por fecha. |
| R-15 | **Cerrado** | `internal/capability` define `Set`/`Overrides`/`Table` con defaults por proveedor y overrides por modelo/glob; `providers.<name>.capabilities` en YAML; `GET /admin/providers` expone el set efectivo. |
| R-16 | **Cerrado** | `Registry.ResolveForChat` y `RemapForFallbackWithCapabilities` filtran weighted/patrón/fallback por capacidades; hedge y streaming también. Fallback sin candidato compatible registra `gateway_capability_mismatches_total` y no degrada tools/visión/JSON. |
| R-17 | **Cerrado** | Validación temprana antes de llamar al proveedor: 400 `model_capability_mismatch` con `param` (`tools`, `messages`, `response_format`, `stream`) para tools/visión/audio/JSON. |
| R-23 | **Parcial** | Strict valida https, hosts privados/loopback/link-local y metadata, con `allow_private`; faltan DNS pinning y allowlist de redirects. |
| R-26 | **Parcial** | Admin key rota sin reiniciar; `/metrics` sigue sin autenticación. |

Todo lo demás (R-02, R-05, R-06, R-08, R-10–R-12, R-14, R-18–R-22, R-24, R-25, R-27–R-30) sigue **pendiente**.

**Fuera de alcance hasta que lo pidan clientes:** operador de Kubernetes, plugins WASM, facturación con Stripe, multi-región activo-activo, fine-tuning, batches, realtime API.

**Dependencias críticas:** R-05 bloquea R-06/R-07/R-08/R-10/R-11. R-10 bloquea R-11/R-12/R-14. R-15 bloquea R-16/R-17. R-18 no debería empezar antes de R-05: un adaptador genérico sin control plane multiplica configuración manual.

---

## 6. Esquema SQL mínimo (Postgres 15+)

Objetivo: control plane persistente y ledger. El data plane **no consulta estas tablas en el hot path**; recibe un snapshot de configuración y emite eventos de uso. Las claves de proveedor se referencian (`secret_ref`), nunca se guardan en claro.

```sql
-- Extensiones: gen_random_uuid() es nativo en PG13+; pgcrypto solo si se usa digest().
create extension if not exists pgcrypto;

create table tenants (
    id          uuid primary key default gen_random_uuid(),
    slug        text not null unique,
    name        text not null,
    status      text not null default 'active'
                check (status in ('active', 'suspended', 'closed')),
    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now()
);

create table api_keys (
    id           uuid primary key default gen_random_uuid(),
    tenant_id    uuid not null references tenants(id) on delete restrict,
    name         text not null,
    key_hash     bytea not null unique,          -- HMAC-SHA256(pepper, key) o SHA-256; nunca la clave en claro
    key_prefix   text not null,                  -- primeros 8 chars para identificar en soporte
    scopes       text[] not null default '{}',
    expires_at   timestamptz,
    revoked_at   timestamptz,
    created_at   timestamptz not null default now(),
    last_used_at timestamptz
);
create index api_keys_tenant_idx on api_keys (tenant_id) where revoked_at is null;

create table providers (
    id          uuid primary key default gen_random_uuid(),
    name        text not null unique,            -- "openai", "groq", "azure-prod"
    kind        text not null,                   -- openai | anthropic | gemini | openai_compatible | azure | bedrock | vertex
    base_url    text not null,
    secret_ref  text not null,                   -- env:OPENAI_API_KEY | vault:path#field
    timeout_ms  integer not null default 30000 check (timeout_ms > 0),
    enabled     boolean not null default true,
    config      jsonb not null default '{}',     -- cabeceras, api-version, deployment, región
    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now()
);

create table models (
    id             uuid primary key default gen_random_uuid(),
    provider_id    uuid not null references providers(id) on delete cascade,
    name           text not null,                -- nombre que ve el cliente
    upstream_name  text,                         -- nombre real si difiere
    capabilities   jsonb not null default '{}',  -- {"chat":true,"stream":true,"tools":true,"vision":false,
                                                 --  "json_schema":true,"embeddings":false,"context_window":128000}
    context_window integer check (context_window is null or context_window > 0),
    enabled        boolean not null default true,
    unique (provider_id, name)
);
create index models_name_idx on models (name);

create table prices (
    id                    uuid primary key default gen_random_uuid(),
    model_id              uuid not null references models(id) on delete cascade,
    currency              text not null default 'USD' check (currency = 'USD'),
    input_per_mtok        numeric(14, 6) not null default 0 check (input_per_mtok >= 0),
    output_per_mtok       numeric(14, 6) not null default 0 check (output_per_mtok >= 0),
    embedding_per_mtok    numeric(14, 6) not null default 0 check (embedding_per_mtok >= 0),
    effective_from        date not null,
    source                text,                  -- URL o "manual"; trazabilidad del precio
    created_at            timestamptz not null default now(),
    unique (model_id, effective_from),
    check (input_per_mtok + output_per_mtok + embedding_per_mtok > 0)
);

-- Rutas: patrón de modelo del cliente -> proveedor/modelo, con peso para canary.
create table routes (
    id          uuid primary key default gen_random_uuid(),
    pattern     text not null,                   -- "gpt-4o", "gpt-4*", "claude-*"
    provider_id uuid not null references providers(id) on delete cascade,
    model_id    uuid references models(id) on delete set null,
    weight      integer not null default 100 check (weight > 0),
    priority    integer not null default 0,
    enabled     boolean not null default true,
    created_at  timestamptz not null default now()
);
create index routes_pattern_idx on routes (pattern) where enabled;

create table budgets (
    id           uuid primary key default gen_random_uuid(),
    tenant_id    uuid not null references tenants(id) on delete cascade,
    period_start date not null,                  -- primer día del mes
    token_limit  bigint check (token_limit is null or token_limit >= 0),
    usd_limit    numeric(14, 6) check (usd_limit is null or usd_limit >= 0),
    created_at   timestamptz not null default now(),
    updated_at   timestamptz not null default now(),
    unique (tenant_id, period_start),
    check (token_limit is not null or usd_limit is not null)
);

-- Ledger append-only. No se actualiza ni se borra; retención por partición/exportación.
create table usage_events (
    id                bigserial primary key,
    event_id          uuid not null default gen_random_uuid() unique, -- idempotencia de ingesta
    request_id        text not null,
    idempotency_key   text,
    tenant_id         uuid not null references tenants(id) on delete restrict,
    api_key_id        uuid references api_keys(id) on delete set null,
    provider_name     text not null,             -- snapshot: sobrevive al borrado del proveedor
    model             text not null,             -- snapshot: nombre solicitado
    prompt_tokens     integer not null default 0 check (prompt_tokens >= 0),
    completion_tokens integer not null default 0 check (completion_tokens >= 0),
    cost_usd          numeric(14, 8) not null default 0 check (cost_usd >= 0),
    status_code       integer not null,
    latency_ms        integer check (latency_ms is null or latency_ms >= 0),
    created_at        timestamptz not null default now()
);
create index usage_events_tenant_time_idx on usage_events (tenant_id, created_at desc);
create index usage_events_request_idx on usage_events (request_id);
create unique index usage_events_idempotency_idx
    on usage_events (tenant_id, idempotency_key)
    where idempotency_key is not null;

create table audit_log (
    id          bigserial primary key,
    actor       text not null,                   -- admin key id, usuario o "system"
    action      text not null,                   -- "tenant.create", "api_key.revoke", "config.patch"
    target_type text not null,
    target_id   text,
    before      jsonb,
    after       jsonb,
    ip          inet,
    created_at  timestamptz not null default now()
);
create index audit_log_created_idx on audit_log (created_at desc);
create index audit_log_target_idx on audit_log (target_type, target_id);
```

**Notas de diseño:**

- `usage_events` crece rápido: en producción, particionar por mes (`partition by range (created_at)`) y exportar antes de borrar. El `bigserial` por partición requiere planificación; alternativa: `uuid` + índice por fecha.
- El ledger no debe bloquear la respuesta. El data plane encola en memoria con límite; si la cola está llena, se descarta el evento y se incrementa una métrica (nunca se bloquea el chat). Un proceso de ingesta idempotente usa `event_id` para evitar duplicados.
- `api_keys.key_hash` sin pepper es aceptable solo si las keys son aleatorias de alta entropía (≥ 128 bits). Si se permiten keys elegidas por el usuario, usar Argon2id.
- `audit_log` es append-only; los cambios de configuración deben escribir `before/after` redactados.
- Migraciones: una herramienta, un directorio `migrations/`, checksum y bloqueo; el arranque del control plane falla si el esquema no está al día.

---

## 7. Criterios de release

Un release se etiqueta solo si:

- [ ] `go test ./... -race` y `go vet ./...` pasan; el contrato OpenAPI valida.
- [ ] La suite de conformidad pasa contra el upstream falso y al menos un SDK oficial.
- [ ] No hay afirmaciones de rendimiento sin hardware, fecha y comando.
- [ ] El CHANGELOG describe cambios verificables; `VERSION` coincide con la etiqueta.
- [ ] El runbook cubre los incidentes de la release (nuevo proveedor, nueva tabla, nueva alerta).
- [ ] Cambios de seguridad tienen test de regresión (SSRF, scopes, admin fail-closed).
- [ ] La migración de esquema está probada desde la versión anterior.

---

## 8. Qué haría primero (orden de ejecución real)

1. **R-02/R-04** (1 semana): congelar la conformidad con SDKs y el e2e; R-01 ya está cerrado. Barato y mejora todo lo demás.
2. **R-05/R-06/R-07** (3–4 semanas): Postgres, tenants y keys. Sin esto no hay producto.
3. **R-10/R-11/R-14** (2–3 semanas): ledger y cuotas. Es lo que convierte “gateway bonito” en “servicio facturable”.
4. **R-18/R-19** (2–3 semanas): proveedor genérico y presets. R-15/R-16/R-17 ya están cerrados; aquí el producto se vuelve útil fuera de OpenAI.
5. **R-23/R-24/R-27/R-28** (2–3 semanas): seguridad y operación. Sin esto no pasa una revisión seria de compra.
6. **R-29/R-30** (2 semanas): entrega instalable y consola mínima.

**Lo que no haría:** añadir 10 proveedores antes de tener ledger y cuotas; montar una UI antes de que el admin API sea estable; optimizar nanosegundos del limiter mientras el control plane es un YAML.

---

## 9. Estado de la sesión 2026-11-13

Commits de la rama `docs/final` (docs) sobre `main` en `f8663d0`; los cambios de código ya estaban mergeados en `main`:

| Área | Qué quedó cerrado | Dónde mirar |
|---|---|---|
| Streaming | `event: error` sin `[DONE]` en fallo mid-stream y en cierre sin evento terminal (`stream_truncated`); usage real; deadline rodante 60 s; `index` en choices/tool calls | `internal/proxy/handler.go`, `internal/provider/sse.go`, `tests/stream_usage_test.go` |
| Errores | Mapeo 400/404/413/429/502/503/504, `Retry-After` hacia arriba, `param` en `unsupported_parameter` | `internal/proxy/errors.go`, `internal/translate/errors.go` |
| Contrato | Campos desconocidos tolerados en data plane; `json_schema` passthrough; body 8 MiB | `internal/proxy/decode.go`, `internal/types/types.go`, `api/openapi.yaml` |
| Límites | Bucket de tokens (`tokens_per_minute`/`token_burst`), overrides aislados, identidad autenticada, Redis | `internal/ratelimit/*`, `internal/proxy/middleware.go` |
| Budget | Precio por token→por millón, leases TTL + reconciliación, `require_shared_store`, meses UTC, cached tokens | `internal/budget/budget.go`, `internal/pricing/*` |
| Caché | `max_bytes`, `max_ttl`, single-flight, semántico sin doble embedding, stats Redis reales | `internal/cache/*` |
| Hot reload | `auth.enabled`, CORS, `logging.level`, admin key/pepper; bind/port/pprof/formato requieren reinicio; watcher con reintento; Redis no bloquea | `cmd/gateway/main.go`, `internal/config/config.go` |
| Auth/seguridad | Scopes fail-closed, allowlist, keys ≥32, pepper, throttle 401, strict mode, validación `base_url`, ETag/audit/rotación admin, pprof opt-in, bind loopback | `internal/auth/*`, `internal/admin/server.go`, `internal/config/config.go`, ADR-005 |
| Capacidades | `capability.Set/Table` con defaults por proveedor y overrides por modelo/glob; 400 `model_capability_mismatch` antes de llamar; fallback/hedge/streaming filtran por capacidad; `/admin/providers` expone el set; métrica `gateway_capability_mismatches_total` | `internal/capability/*`, `internal/proxy/router.go`, `internal/proxy/handler.go` |
| Idempotencia | `Idempotency-Key` en chat no-stream y embeddings; replay 2xx con `Idempotent-Replayed`; 409 en reuso o duplicado en vuelo; store LRU acotado + Redis cross-réplica; métrica `gateway_idempotency_requests_total` | `internal/idempotency/*`, `cmd/gateway/main.go` |
| CI/deploy | golangci-lint v2, cobertura `-coverpkg` + gate 55 %, Redis service, gate OpenAPI (Redocly cero warnings + oasdiff breaking), multiarch+SBOM+cosign, NetworkPolicy/SA/topology, perfil prod, `GATEWAY_ENV`/`GATEWAY_REPLICAS`, ConfigMap con `redis_url` | `.github/workflows/*`, `deploy/k8s/*`, `docker-compose.yaml` |
| Docs | README/RUNBOOK/ARCHITECTURE/OpenAPI/SECURITY/CONTRIBUTING/BENCH/ROADMAP sincronizados; ADR-005; cobertura medida 66.0 % y benchmarks locales | este repo |
| Tests | Cobertura real 66.0 % (`-coverpkg=./...`, 2026-11-13, `f8663d0`); suite de integración Redis con `-tags=integration` (requiere `REDIS_URL`) | `go test ./... -coverpkg=./...`, `go test -tags=integration ./...` |

**Lo que sigue pendiente y no se debe presentar como hecho:** Postgres/control plane persistente, CRUD de tenants/keys, ledger de uso y reconciliación, cuotas por tenant contra ledger, proveedor OpenAI-compatible genérico y adaptadores enterprise (Azure/Bedrock/Vertex), UI, SLOs medidos, load/chaos testing, `/metrics` autenticado, secret manager, PII, mTLS, Helm/Terraform y e2e en kind.

## 10. Enlaces

- [`ARCHITECTURE.md`](ARCHITECTURE.md) — cómo funciona hoy.
- [`RUNBOOK.md`](RUNBOOK.md) — operación e incidentes.
- [`../SECURITY.md`](../SECURITY.md) — reporte y modelo de amenazas.
- [`../api/openapi.yaml`](../api/openapi.yaml) — contrato HTTP.
- [`adr/ADR-004-professionalization.md`](adr/ADR-004-professionalization.md) — decisiones de este plan.
- [`adr/ADR-005-fail-closed-auth-and-strict-mode.md`](adr/ADR-005-fail-closed-auth-and-strict-mode.md) — auth fail-closed y modo strict implementados.
