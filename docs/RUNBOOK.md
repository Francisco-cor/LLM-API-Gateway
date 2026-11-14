# RUNBOOK — operación del gateway

> **Audiencia:** quien esté de guardia con acceso al cluster, al admin API y a Redis/Prometheus.
> **Principio:** primero confirmar el síntoma con datos, después mitigar, después entender. No reinicies “por si acaso”: el gateway drena y el estado de budget/limiter vive en Redis o en memoria.
> SLOs y alertas de este documento son **propuestos**: el gateway todavía no mide todos los SLI necesarios (ver §7).

---

## 1. Mapa de componentes

| Componente | Puerto por defecto | Qué hace | Dónde mirar |
|---|---|---|---|
| Gateway HTTP | `8080` | `/v1/*`, `/health*`, `/metrics` | `cmd/gateway/main.go` |
| Admin API | `8081`, bind `127.0.0.1` por defecto | `/admin/*`, pprof opt-in | `internal/admin/server.go` |
| pprof | **no tiene puerto propio** | Rutas `/debug/pprof/*` dentro del admin, solo si `admin.pprof_enabled: true` | `internal/admin/server.go` |
| Redis | `6379` | rate limit distribuido, cache, budget | `internal/redisstore` |
| Postgres | — | **no existe todavía** (roadmap fase 2) | `docs/ROADMAP.md` |

**Gotcha de Docker Compose:** `docker-compose.yaml` publica `8081`, pero `config.yaml` no define `admin.port`/`admin.bind_address`; el default es `127.0.0.1:8081` **dentro del contenedor**. Desde el host no se alcanza. Para operar el admin en local usa `config.dev.yaml` o define `admin.bind_address: 0.0.0.0` en un perfil propio y protégelo con red/firewall. En Kubernetes, `deploy/k8s/configmap.yaml` fija `0.0.0.0` y el Service `llm-api-gateway-admin` es `ClusterIP`.

---

## 2. Arranque y comprobaciones básicas

```bash
# local
go run ./cmd/gateway -config config.dev.yaml

# binario
make build && ./bin/gateway -config config.dev.yaml

# docker (el contenedor trae su propio config.yaml)
docker build -t llm-api-gateway .
docker run --rm -p 8080:8080 \
  -e OPENAI_API_KEY="$OPENAI_API_KEY" \
  -e ADMIN_API_KEY="$ADMIN_API_KEY" \
  llm-api-gateway

# compose (usa .env y config.yaml)
docker compose up --build -d
docker compose ps
docker compose logs -f gateway
```

Comprobaciones en orden:

```bash
curl -sS http://localhost:8080/health | jq
# {"status":"ok","uptime_seconds":...,"start_time":"...","ready":true}

curl -sS -o /dev/null -w '%{http_code}\n' http://localhost:8080/livez   # 200
curl -sS -o /dev/null -w '%{http_code}\n' http://localhost:8080/readyz  # 200 si hay >=1 provider sano

curl -sS http://localhost:8080/health/providers | jq
curl -sS http://localhost:8080/v1/models | jq '.data[].id'

# si auth está activo
curl -sS http://localhost:8080/v1/models \
  -H "Authorization: Bearer $GATEWAY_API_KEY" | jq '.object'
```

Semántica exacta:

- `/health` y `/livez` devuelven **200 siempre** mientras el proceso viva; `ready` refleja el drenaje.
- `/readyz` devuelve **503** si `ready=false` (drenando) o si **0 proveedores** están sanos; en caso contrario 200. Cachea el resultado `health.readiness_cache_ttl` (15 s por defecto, 30 s en `config.prod.yaml`).
- `/health/providers` devuelve **200 incluso con `overall: degraded`**; no es un probe de Kubernetes. Con `health.skip_expensive_checks: true`, Anthropic aparece como `skipped`; `/readyz` lo cuenta como sano, pero `/health/providers` marca `overall: degraded` porque cualquier estado distinto de `healthy` degrada el agregado. **Es esperado, no es un incidente.**
- `/metrics` es Prometheus y **no está autenticado** (el middleware de auth lo excluye). En producción, restringe el acceso por red o métrica interna (roadmap R-26).

---

## 3. Recarga de configuración y rollback

Tres caminos, todos validan antes de aplicar:

```bash
# 1. admin API (lee el archivo configPath del proceso)
curl -sS -X POST http://127.0.0.1:8081/admin/reload \
  -H "Authorization: Bearer $ADMIN_API_KEY" | jq
# {"status":"reloaded"}  | 400 {"error":"..."} con la config anterior intacta

# 2. SIGHUP
kill -HUP "$(pgrep -f 'gateway -config')"

# 3. watcher de archivo (poll 1 s + debounce 100 ms)
$EDITOR config.yaml   # al guardar, recarga sola
```

Si la recarga falla:

1. El error se registra como `admin reload failed`, `SIGHUP reload failed` o `watch reload failed, keeping old config`; el watcher añade `config watcher retrying` y reintenta con backoff (hasta 10 s) sin perder el cambio pendiente.
2. La configuración anterior sigue activa: `applyConfig` valida y construye proveedores/precios/Redis **antes** de mutar el registro y los handlers.
3. Un fallo de Redis **ya no aborta la recarga**: `redisStore.ReplaceURL` solo se intenta cuando la URL cambió y, si falla, se registra `redis backend update failed; keeping previous backend`, se conserva la conexión previa y el resto de knobs (auth, límites, proveedores, caché) se aplica igualmente. El budget fallará cerrado si está configurado para ello.
4. Corrige el archivo y repite; no hace falta reiniciar.

Rollback real (config válida pero incorrecta):

```bash
git diff -- config.yaml                 # o usa tu copia/backup
git checkout -- config.yaml             # si el archivo está versionado
curl -sS -X POST http://127.0.0.1:8081/admin/reload \
  -H "Authorization: Bearer $ADMIN_API_KEY"
```

**Importante:** `POST /admin/reload`, `SIGHUP` y el watcher **no** pueden cambiar `server.port`, `server.read_timeout`/`write_timeout`, `admin.port`, `admin.bind_address`, `admin.pprof_enabled` ni `logging.format`: se rechazan con un error claro (`... requires restart`). `logging.format` es la excepción: registra un warning y mantiene el handler actual. La **admin key sí se rota en caliente** (§4.6). También se recargan `auth.enabled`, la pepper, CORS, `logging.level`, rate limit (incluido token bucket), cache, resiliencia, rutas, health, budget, pricing y proveedores. `PATCH /admin/config` solo expone rate limit, cache, health, resiliencia, routing y logging; auth/CORS/proveedores se cambian editando el archivo y recargando.

### 3.1 Modo strict

`--strict` o `GATEWAY_ENV=prod` aplican una validación adicional **al arrancar y en cada recarga**. Si falla, el proceso no arranca o la recarga se rechaza conservando la config anterior. Exige:

- `auth.enabled: true`, keys de ≥ 32 caracteres con al menos un scope.
- `admin.api_key` presente, `admin.allow_insecure: false`, `admin.pprof_enabled: false`.
- CORS sin `*`.
- `base_url` https, sin hosts locales/privados/loopback/link-local ni metadata (`169.254.169.254`, `100.100.100.200`, `fd00:ec2::254`) salvo `allow_private: true`; los metadata hosts se bloquean siempre.
- `REDIS_URL` cuando rate limit o budget están activos, y `rate_limit.budget.require_shared_store: true` si hay budget.

Los K8s manifests ya definen `GATEWAY_ENV=prod`; Compose lo usa en el perfil `prod`. Para reproducir un fallo típico:

```bash
GATEWAY_ENV=prod ./bin/gateway -config config.dev.yaml
# strict config validation: strict validation failed:
#   auth.enabled must be true in strict mode
#   ...
```

No hay un flag para “degradar” strict en caliente: quitar `GATEWAY_ENV` exige reiniciar. Si el problema es una recarga, corrige el YAML y vuelve a recargar.

Rollback de despliegue:

```bash
kubectl -n <ns> rollout history deploy/llm-api-gateway
kubectl -n <ns> rollout undo deploy/llm-api-gateway
kubectl -n <ns> rollout status deploy/llm-api-gateway --timeout=120s
```

---

## 4. Incidentes

### 4.1 Redis caído o inalcanzable

**Síntomas:** errores `redis ping: ...` al arrancar o recargar; logs de arranque `redis backend unavailable, using local fallbacks`; latencia de rate limit/cache alterada; budget devolviendo 503.

**Qué hace el gateway, por subsistema:**

| Subsistema | Comportamiento | Consecuencia |
|---|---|---|
| Rate limit | `RedisLimiter` cae al limiter local del proceso y marca `degraded` internamente; no hay métrica expuesta para ese flag | Cada réplica aplica su propio cupo: el límite global efectivo se multiplica por el número de réplicas |
| Cache | Si Redis estaba activo, `Get` falla → miss y `Set` falla en silencio; **no hay fallback automático a memoria** | Se pierde el ahorro; el tráfico sigue |
| Budget | Con `require_shared_store: true` (recomendado y obligatorio en strict), `Reserve` devuelve `ErrUnavailable` → **503 `budget_unavailable`** (fail-closed). Sin ese flag, cae a contabilidad por proceso y multiplica la cuota por réplica | No se permite gasto sin control cuando el operador lo exige |

Diagnóstico:

```bash
redis-cli -u "$REDIS_URL" ping
redis-cli -u "$REDIS_URL" info clients | head
redis-cli -u "$REDIS_URL" --scan --pattern 'ratelimit:*' | head
redis-cli -u "$REDIS_URL" --scan --pattern 'budget:*' | head
kubectl -n <ns> logs deploy/llm-api-gateway --since=15m | grep -Ei 'redis|budget_unavailable'
```

Mitigación:

- Si el budget está en 503 y el negocio prefiere disponibilidad, **no desactives el budget a ciegas**: cambia temporalmente `rate_limit.budget.enabled: false` y recarga, dejando constancia; o restaura Redis. Un budget desactivado es gasto sin límite.
- Si el rate limit se dispara por réplicas, restaura Redis; el fallback local no es una solución de capacidad.
- Si el cache era crítico, reinicia con Redis sano: `buildCache` elige Redis solo si el store está activo al construir/recargar; no se cambia en caliente de Redis a memoria sin recarga.

Recuperación: al volver Redis, las operaciones exitosas limpian `degraded` en el limiter automáticamente. Para el cache, fuerza una recarga (`POST /admin/reload`) para reconstruir el backend si el store ya está activo. La recarga no se bloquea por Redis: los knobs que no dependen de él se aplican aunque el ping falle; corrige `redis_url` y vuelve a recargar cuando Redis esté disponible.

### 4.2 Proveedor devolviendo 429 / 5xx

**Síntomas:** `X-Gateway-Provider` apunta a un proveedor concreto; `gateway_provider_errors_total` sube; el cliente recibe el estado que corresponda al fallo. El gateway ya **no colapsa todos los errores en 502**:

| Upstream | Respuesta del gateway | `type` / `code` |
|---|---|---|
| 400 / 422 | **400** | `invalid_request_error` (o `unsupported_parameter` con `param`) |
| 404 | **404** | `model_not_found` |
| 413 | **413** | `request_too_large` |
| 429 | **429** | `rate_limit_error` / `rate_limit_exceeded`; `Retry-After` redondeado hacia arriba |
| 401 / 403 | **502** | `provider_error` / `provider_auth_error` (nunca se reenvía el cuerpo de auth del upstream) |
| 503 / 504 | **503** | `provider_error` / `provider_unavailable` |
| resto de 5xx / red | **502** | `provider_error` / `provider_error` |

`Retry-After` se redondea hacia arriba al segundo y nunca baja de 1. Un 429 del proveedor **sí se devuelve como 429** si se agotan los fallbacks; el 429 local del limiter usa `rate_limit_exceeded` y el del budget `insufficient_quota`.

Diagnóstico:

```bash
# tasa de errores por proveedor
curl -sS http://localhost:8080/metrics | \
  grep -E 'gateway_provider_errors_total|gateway_requests_total'

# circuit breaker: 0=closed, 1=open, 2=half-open
curl -sS http://localhost:8080/metrics | grep gateway_circuit_state

# logs recientes
kubectl -n <ns> logs deploy/llm-api-gateway --since=10m | \
  grep -E 'provider failed|fallback|circuit open|429|5[0-9][0-9]'
```

Mitigación:

1. Confirma si es un proveedor o todos: mira `X-Gateway-Provider` y el label `provider`.
2. Si es uno solo y hay fallback sano, el gateway ya lo está usando. Si no hay fallback, añade otro proveedor al `fallback_chain` y recarga (requiere key/config).
3. Si el proveedor está en 429 sostenido, reduce tráfico o sube el límite en el proveedor; el `Retry-After` del upstream se propaga al cliente.
4. Para retirar temporalmente un proveedor: quita su key del entorno/config, ajusta `fallback_chain` y recarga. No hay flag `enabled` por proveedor en esta versión.
5. Si el problema es de credenciales (401/403), el circuito puede abrirse; rota la key (`providers.<name>.api_key`) y recarga.

### 4.3 Circuit breaker abierto

**Síntoma:** logs `circuit open, skipping primary/fallback`; `gateway_circuit_state{provider="..."} 1`; tráfico desviado o 502 si no hay alternativas.

- Se abre tras `resilience.circuit.failure_threshold` fallos (5 por defecto), permanece `open_timeout` (30 s) y pasa a `half-open` con 1 probe (`half_open_max=1`).
- **No hay endpoint para resetearlo.** Se cierra solo con un probe exitoso o reiniciando el proceso. Reiniciar solo uno de varios pods deja estados distintos: es aceptable porque el breaker es local.
- Si el probe de half-open falla, vuelve a abrir. Verifica la causa (key, cuota, 5xx) antes de reiniciar.

```bash
curl -sS http://localhost:8080/metrics | grep 'gateway_circuit_state'
kubectl -n <ns> logs deploy/llm-api-gateway --since=10m | grep -i circuit
```

### 4.4 Presupuesto agotado (`429 insufficient_quota`) o backend no disponible (`503 budget_unavailable`)

**Síntomas:** 429 con `{"error":{"code":"insufficient_quota",...}}`; o 503 `budget_unavailable` cuando Redis está configurado y `require_shared_store: true`. El budget es mensual por tenant, con meses **UTC** (`YYYY-MM`); sin Redis compartido es por réplica.

Diagnóstico:

```bash
# claves de budget: hash del tenant + mes UTC. Solo lectura; no editar a mano.
redis-cli -u "$REDIS_URL" --scan --pattern 'budget:*'
redis-cli -u "$REDIS_URL" hgetall 'budget:<hash>:2026-11'
# tokens / usd consumidos y reservas pendientes
```

- `rate_limit.budget.enabled`, `monthly_tokens`, `monthly_usd`, `cost_per_token_usd`, `require_shared_store`, `reservation_ttl` y el catálogo `pricing` están en config.
- Los precios del catálogo son **USD por 1M tokens**; `cost_per_token_usd` se convierte a por-millón al construir el catálogo (0.00001 USD/token = 10 USD/1M). Los tokens de prompt cacheados se cobran a `cache_read_per_million_tokens_usd` si está definido.
- Las reservas son **leases**: si un `Commit`/`Cancel` no llega, la cuota se recupera tras `reservation_ttl` (10m por defecto). El reconciliador recorre cada minuto los tenants con keys y libera leases vencidas; no hay que borrar claves a mano.
- `pricing.unknown_model_policy: reject` produce `503 pricing_unavailable` en vez de estimar; revisa que el modelo tenga precio.
- El ajuste posterior (`CommitWithRetry`) puede fallar si el consumo real supera la estimación; queda en logs como `budget adjustment exceeded estimate` o `stream budget adjustment exceeded estimate`, pero la llamada ya se hizo. La lease vence sola si no se pudo commitear.
- Cambiar límites a media mes no borra el consumo: `UpdateConfig` conserva el uso; bajar el límite puede dejar al tenant inmediatamente por encima.
- `require_shared_store: true` es obligatorio en strict y recomendado con >1 réplica. Sin él, `GATEWAY_REPLICAS>1` emite un warning `budget enabled without require_shared_store...`.
- Para una emergencia, sube `monthly_usd`/`monthly_tokens` y recarga; no borres la clave de Redis salvo que aceptes reiniciar el contador del mes (y documenta el incidente).

### 4.5 Stream SSE cortado o truncado

**Contrato:** un stream completo termina con `finish_reason`/usage del proveedor y `data: [DONE]`. Si el proveedor corta la conexión o cierra sin evento terminal, el gateway emite:

```text
event: error
data: {"error":{"message":"provider stream truncated","type":"provider_error","code":"stream_truncated"}}
```

y **no emite `[DONE]`**. `[DONE]` significa éxito; un cliente que lo viera tras una truncación registraría una respuesta incompleta como buena. Un fallo mid-stream del proveedor sigue el mismo camino (`event: error`, sin `[DONE]`).

**Síntomas:** el cliente recibe chunks y luego `event: error` sin `[DONE]`; log `stream provider error` o `stream truncated before terminal event`; `gateway_provider_errors_total{code="stream_truncated"}` sube.

Causas típicas:

- El proveedor cortó la conexión o superó su timeout (`providers.<name>.timeout`, 30 s por defecto).
- El cliente desconectó: el handler termina por `r.Context().Done()` y **no** hay reanudación (no hay resume por `Last-Event-ID`).
- El cliente dejó de leer: el deadline de escritura es **rodante de 60 s** y se renueva antes de cada chunk; si no hay escritura posible, se corta en vez de pinchar el proveedor indefinidamente.
- Error antes del primer chunk: se intenta fallback; si todos fallan, el estado mapeado (400/404/413/429/502/503/504) se devuelve antes de abrir SSE.
- El uso real llega en el chunk final del proveedor cuando `stream_options.include_usage` está activo; si nunca llega, el budget commitea la estimación y deja el warning `stream ended without provider usage`.

Qué hacer:

```bash
kubectl -n <ns> logs deploy/llm-api-gateway --since=10m | \
  grep -E 'stream provider error|stream truncated|stream completed|stream circuit open'
curl -sS http://localhost:8080/metrics | grep -E 'gateway_stream_(ttft|duration)_seconds|gateway_provider_errors_total'
```

- Si es un proveedor, el fallback ya se intentó antes del primer byte. Un stream ya iniciado **no se puede mover** a otro proveedor sin romper el contrato; el cliente debe reintentar y tratar `event: error` como fallo.
- No hay caché para streaming (`stream:true` nunca se cachea), así que un reintento vuelve a costar.
- Si el patrón es sistémico, reduce `max_tokens` o sube timeouts; documenta el proveedor y la hora.

### 4.6 Rotación de claves

**Keys de tenant (`auth.keys`):** se recargan en caliente.

1. Añade la key nueva con el mismo `tenant`/`scopes` (≥ 32 caracteres en producción), sin quitar la vieja.
2. `POST /admin/reload` o `SIGHUP`.
3. Migra clientes.
4. Marca la vieja como `expires_at` pasada o elimínala y recarga.
5. Verifica 401 con la vieja y 200 con la nueva.

**Pepper (`auth.key_pepper` / `AUTH_KEY_PEPPER`):** se re-hashean las keys al recargar; cambiar la pepper no requiere migración en disco, pero invalida cualquier hash calculado con la anterior. Rota pepper y keys en la misma recarga para no dejar una ventana con hashes mixtos.

**Admin key (`ADMIN_API_KEY`):** se **rota en caliente**. `applyConfig` llama a `adminSrv.SetAPIKey` y `SetRequireAuth`; no hace falta reiniciar ni hacer rolling por la key en sí. Procedimiento:

1. Añade la key nueva al Secret/entorno del que se expande `admin.api_key` y recarga (o usa `PATCH`/`SIGHUP` con el archivo actualizado).
2. Comprueba `GET /admin/config` con la nueva (200) y con la vieja (401).
3. Retira la vieja y recarga otra vez.
4. Durante la ventana, ambas no conviven: el swap es atómico por proceso; con varias réplicas cada una cambia cuando recarga. Si necesitas solape entre réplicas, haz la rotación en dos fases y verifica réplica a réplica.

**Keys de proveedor (`providers.<name>.api_key`):** se recargan si vienen de variables de entorno expandidas al leer el archivo; el proceso no relee el entorno, así que en la práctica reinicia el pod o usa un secret manager (roadmap R-24). En Docker/K8s, actualiza el Secret y haz rolling update.

**Verificación:**

```bash
curl -sS -o /dev/null -w '%{http_code}\n' http://localhost:8080/v1/models \
  -H "Authorization: Bearer $NEW_KEY"   # 200
curl -sS -o /dev/null -w '%{http_code}\n' http://localhost:8080/v1/models \
  -H "Authorization: Bearer $OLD_KEY"   # 401 tras la retirada
```

### 4.7 Profiling (pprof)

pprof está **deshabilitado por defecto** y, cuando se habilita, vive en el mux del admin (`:8081`), no en `:6060`. Requiere reinicio porque las rutas se registran al construir el handler.

```yaml
admin:
  pprof_enabled: true
  api_key: ${ADMIN_API_KEY}
  bind_address: 127.0.0.1   # o 0.0.0.0 solo si hay red/ACL
```

```bash
# heap (30s de muestreo por defecto no; heap es instantáneo)
curl -sS -H "Authorization: Bearer $ADMIN_API_KEY" \
  'http://127.0.0.1:8081/debug/pprof/heap' -o /tmp/heap.prof
go tool pprof -http=:9091 /tmp/heap.prof

# CPU 30s
curl -sS -H "Authorization: Bearer $ADMIN_API_KEY" \
  'http://127.0.0.1:8081/debug/pprof/profile?seconds=30' -o /tmp/cpu.prof
go tool pprof -http=:9091 /tmp/cpu.prof

# goroutines (texto)
curl -sS -H "Authorization: Bearer $ADMIN_API_KEY" \
  'http://127.0.0.1:8081/debug/pprof/goroutine?debug=1'
```

Un heap dump contiene claves de proveedor y prompts en memoria: trátalo como secreto, no lo subas a un ticket y bórralo al terminar. Deshabilita pprof y reinicia cuando acabes.

### 4.8 Drenaje y apagado

`SIGTERM`/`SIGINT`:

1. `health.SetReady(false)` → `/readyz` 503.
2. Espera 5 s para que kube-proxy/ LB retiren el endpoint.
3. `Shutdown` del admin (5 s) y del gateway (30 s).
4. Cierra limiter/Redis.

Si el pod tarda más de `terminationGracePeriodSeconds: 35`, revisa streams largos: el shutdown espera hasta 30 s y luego corta. No reduzcas el grace period por debajo de 35 s sin medir.

### 4.9 Admin: ETag, `If-Match` y auditoría

`GET /admin/config` devuelve un `ETag` opaco que identifica la configuración efectiva. `PATCH /admin/config` acepta `If-Match`:

```bash
etag=$(curl -sS -D - -o /tmp/cfg.json http://127.0.0.1:8081/admin/config \
  -H "Authorization: Bearer $ADMIN_API_KEY" | awk -F': ' 'tolower($1)=="etag"{print $2}' | tr -d '\r')

curl -sS -X PATCH http://127.0.0.1:8081/admin/config \
  -H "Authorization: Bearer $ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -H "If-Match: $etag" \
  -d '{"rate_limit":{"requests_per_minute":120}}'
# 200 {"status":"patched","etag":"..."}
```

- Si el `ETag` no coincide, la respuesta es **409** con `{"error":"config version does not match If-Match; reload and retry","etag":"<actual>"}`. Vuelve a leer, reaplica el cambio y reintenta. Sin `If-Match` (o con `*`) el PATCH sobrescribe, como antes.
- Los PATCH se serializan con un mutex: el read-modify-apply no se entrecruza con otro PATCH.
- Cada intento autenticado (éxito, fallo, bloqueo, reload, PATCH, conflicto) emite un registro `admin_audit` con actor fingerprint, acción y detalle. Los secretos y valores del patch se resumen/redactan. Busca `admin_audit` en logs:
  ```bash
  kubectl -n <ns> logs deploy/llm-api-gateway --since=1h | grep admin_audit
  ```
- El rate limit de fallos de admin es por IP de proceso (20 fallos/minuto → 429 con `Retry-After`). No es estado compartido.

### 4.10 Scopes, 401 y 403

Con `auth.enabled: true`:

- Cada ruta `/v1/*` tiene un scope obligatorio: `POST /v1/chat/completions` → `chat:write`; `POST /v1/embeddings` → `embeddings:write`; `GET /v1/models` → `models:read`. Una ruta `/v1/*` no mapeada se **deniega por defecto** (403), no se trata como pública.
- Una key sin el scope recibe **403** con `WWW-Authenticate: Bearer error="insufficient_scope", scope="..."` y `code: insufficient_scope`. No es un problema de credencial: no rotes la key, corrige los scopes.
- Key ausente, inválida o expirada → **401** `invalid_api_key`. Tras 20 fallos por IP en un minuto, el gateway devuelve **429** `too_many_authentication_failures` con `Retry-After` (estado local al proceso).
- `auth.enabled` se puede activar/desactivar con `SIGHUP`/reload; el middleware ya está instalado.
- El tenant se deriva **solo** de la key autenticada: se ignora y se sobrescribe cualquier `X-Tenant-ID` del cliente. Con auth desactivada, `X-Tenant-ID` es controlado por el cliente y solo es aceptable en desarrollo.
- Las keys deben tener ≥ 32 caracteres si `admin.allow_insecure` es false. En dev se permite una key corta; en strict nunca.

### 4.11 Cuerpos grandes (413)

Chat y embeddings aceptan hasta **8 MiB** (`MaxBytesReader`), pensado para visión/audio. Por encima, la respuesta es **413** `invalid_request_error` con `request body too large (max 8MiB)`, antes de parsear el JSON. El PATCH de admin mantiene su propio límite de 1 MiB. El data plane tolera campos desconocidos; el plano de control (config, admin) sigue rechazándolos.

---

## 5. Diagnóstico rápido con métricas

Métricas reales (`internal/metrics/metrics.go`):

| Métrica | Tipo | Labels | Uso |
|---|---|---|---|
| `gateway_requests_total` | counter | `method,path,status,provider` | tráfico, errores, rutas |
| `gateway_request_duration_seconds` | histogram | `method,path,provider` | latencia total (incluye upstream) |
| `gateway_tokens_total` | counter | `provider,type` (`prompt\|completion\|total`) | consumo |
| `gateway_cost_usd_total` | counter | `provider,model,pricing` (`known\|fallback`) | coste estimado |
| `gateway_provider_errors_total` | counter | `provider,code` | errores 5xx por proveedor |
| `gateway_circuit_state` | gauge | `provider` (0/1/2) | breaker |
| `gateway_cache_hits_total` | counter | `result` (`hit\|miss`) | efectividad |
| `gateway_cache_size` | gauge | — | entradas (memoria; Redis cuenta claves con SCAN cacheado 30 s) |
| `gateway_stream_ttft_seconds` | histogram | `provider` | time to first token SSE |
| `gateway_stream_duration_seconds` | histogram | `provider` | duración del stream, incluidos los cortados |

Consultas útiles:

```promql
# error rate global 5xx
sum(rate(gateway_requests_total{status=~"5.."}[5m]))
  / sum(rate(gateway_requests_total[5m]))

# 429 por rate limit vs quota
sum by (status) (rate(gateway_requests_total{status="429"}[5m]))

# p95 total (incluye el proveedor)
histogram_quantile(0.95,
  sum by (le) (rate(gateway_request_duration_seconds_bucket[5m])))

# coste por hora y por modelo
sum by (model) (increase(gateway_cost_usd_total[1h]))

# circuit abierto
max by (provider) (gateway_circuit_state) == 1

# cache hit ratio
sum(rate(gateway_cache_hits_total{result="hit"}[15m]))
  / sum(rate(gateway_cache_hits_total[15m]))

# streams truncados por proveedor
sum by (provider) (rate(gateway_provider_errors_total{code="stream_truncated"}[10m]))
```

**No existe** hoy una métrica de budget restante, de tamaño de cola, de Redis up/down ni de latencia separada del upstream. No inventes paneles con esos nombres; están en el backlog (R-14, R-27, R-28).

---

## 6. SLOs propuestos

No medidos todavía; se publican para orientar la instrumentación y las alertas. Ajustar tras 30 días de datos.

| SLI | Definición | Objetivo | Ventana | Nota |
|---|---|---|---|---|
| Disponibilidad de API | `1 - (5xx / total)` en `/v1/*`, excluyendo 429 y errores 4xx del cliente | 99,9 % | 30 días | Un 502 por agotar fallbacks cuenta como no disponible |
| Readiness | `/readyz` responde 200 | 99,95 % | 30 días | Requiere blackbox exporter |
| Latencia de overhead | p95 de `gateway_request_duration_seconds` menos latencia del upstream | < 50 ms | 7 días | **No medible hoy**: el histograma incluye upstream; hace falta un timer interno |
| Corrección de cuota | desvío ledger vs budget | < 0,5 % | diario | Requiere ledger (fase 3) |
| Propagación de config | tiempo hasta que todas las réplicas ven una versión | < 5 s p95 | 30 días | Requiere versión en el snapshot (fase 2) |
| Frescura del ledger | eventos perdidos / aceptados | < 0,1 % | 7 días | Requiere buffer + métrica de descarte |

Política de error budget: si se consume > 50 % del budget mensual en 7 días, se congelan cambios no urgentes y se prioriza estabilidad; si se consume el 100 %, solo se despliegan fixes.

---

## 7. Alertas propuestas (Prometheus)

Ajusta `job`, namespace y umbrales. Sin `for:` generoso, el ruido hace que nadie mire.

| Alerta | Expresión | Umbral | Acción |
|---|---|---|---|
| `GatewayDown` | `up{job="llm-api-gateway"} == 0` | 2 m | Comprobar pod, logs, `/health`; reiniciar si el proceso murió |
| `ReadinessFailing` | `probe_success{job="llm-gateway-readyz"} == 0` | 5 m | `/readyz`, `/health/providers`, Redis, claves |
| `High5xxRate` | 5xx / total | > 2 % durante 10 m | §4.2; distinguir 502 de proveedor vs bug |
| `ProviderErrorSpike` | `sum by (provider)(rate(gateway_provider_errors_total[5m]))` | > 0,1/s durante 10 m | Identificar proveedor; fallback; estado upstream |
| `CircuitOpen` | `max by (provider)(gateway_circuit_state) == 1` | 5 m | §4.3; no reiniciar en bucle |
| `RateLimitSpike` | 429 / total | > 20 % durante 10 m | Puede ser abuso o límite mal dimensionado; revisar tenant |
| `LatencyHigh` | p95 total | > 5 s durante 10 m | Depende del proveedor; comparar con su status |
| `CostBurnHigh` | `sum(increase(gateway_cost_usd_total[1h])) * 24` | > 30 % del budget mensual configurado | Revisar tenants/modelos; el agregado no es por tenant |
| `UsageLedgerDropped` | `rate(gateway_usage_events_dropped_total[5m])` | > 0 | **Métrica futura** (fase 3); si existe, investigar buffer/Postgres |
| `RedisDown` | `redis_up == 0` (redis_exporter) | 2 m | §4.1; el budget fallará cerrado si `require_shared_store: true` |
| `StreamTruncated` | `sum by (provider)(rate(gateway_provider_errors_total{code="stream_truncated"}[10m]))` | > 0 durante 10 m | §4.5; el cliente recibe `event: error` sin `[DONE]` |
| `AdminAuthFailures` | `count_over_time` de `admin_audit` con `action=auth_failure` | > 10 en 10 m | §4.9; revisar IP/key, no rotar a ciegas |
| `CacheHitRatioLow` | hit ratio con tráfico > 1 req/s | < 10 % durante 1 h | Solo si la caché está habilitada; revisar TTL/keys |

Comandos de verificación por alerta:

```bash
kubectl -n <ns> get pods -l app=llm-api-gateway
kubectl -n <ns> describe pod <pod>
kubectl -n <ns> logs <pod> --since=15m
curl -sS http://<pod>:8080/health | jq
curl -sS http://<pod>:8080/readyz | jq
curl -sS http://<pod>:8080/metrics | grep -E 'gateway_(requests_total|provider_errors_total|circuit_state)'
```

---

## 8. Checklist de guardia

- [ ] ¿El fallo es de gateway, de proveedor o de Redis? (`/readyz`, `/health/providers`, `X-Gateway-Provider`, logs)
- [ ] ¿Afecta a todos los tenants o a uno? (429 `insufficient_quota`, 403 `insufficient_scope`, key concreta)
- [ ] Si es un 403, ¿es scope o allowlist de ruta? No rotes la key; corrige `auth.keys[].scopes`.
- [ ] ¿Hay un cambio reciente de config? (`git log`, `admin reload` en logs, hora del incidente)
- [ ] ¿Se puede mitigar sin reiniciar? (fallback, subir límite, recargar, rotar admin key en caliente)
- [ ] Si se reinicia: ¿se pierde el budget local? (sí, si no hay Redis; anótalo)
- [ ] ¿Se ha documentado el incidente y la mitigación? ¿Hace falta una alerta nueva?
