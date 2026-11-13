# ADR-005: Auth fail-closed y modo strict

**Status:** Accepted (implementado en `v1.1.1`)
**Date:** 2026-11-13 · **Deciders:** maintainer
**Supersedes:** nada. Desarrolla las decisiones 4 y 5 de ADR-004 para el estado actual.
**Related:** [`ADR-004-professionalization.md`](ADR-004-professionalization.md), [`../RUNBOOK.md`](../RUNBOOK.md), [`../../SECURITY.md`](../../SECURITY.md)

## Contexto

El gateway puede reescribir configuración, guardar claves de proveedor y exponer pprof. Hasta `v1.1.0`, varias decisiones eran “fail-open por comodidad”:

- un admin sin key arrancaba si `allow_insecure` quedaba en el valor por defecto;
- un tenant key sin `scopes` podía autenticarse y llegar al data plane;
- un scope desconocido se ignoraba;
- el rate limit de fallos de auth no existía;
- `base_url` no se validaba (SSRF);
- una configuración de producción incompleta (sin Redis, sin auth, con CORS `*`) solo se detectaba en revisión manual.

En un despliegue multi-réplica, esos defaults convierten un error de configuración en una superficie abierta o en una cuota multiplicada. La política de ADR-004 ya decía “fail-closed donde hay dinero o control”; este ADR la hace ejecutable.

## Decisiones

### 1. Scopes obligatorios y allowlist

- `auth.enabled: true` exige que cada key declare al menos un scope. `auth.require_scopes` por defecto es `true`; ponerlo a `false` es una decisión explícita para una key deny-all.
- Los scopes válidos son una allowlist: `chat:write`, `embeddings:write`, `models:read`, `admin:write`, `resource:<name>`, `*`. Un typo es un error de arranque, no un permiso nuevo.
- La tabla ruta→scope es una allowlist: `GET /v1/models` → `models:read`, `POST /v1/chat/completions` → `chat:write`, `POST /v1/embeddings` → `embeddings:write`. Cualquier otra ruta `/v1/*` se deniega por defecto (403), incluidas rutas futuras que alguien olvide registrar.
- El tenant se deriva **solo** de la key autenticada; `X-Tenant-ID` del cliente se ignora y se sobrescribe. Sin auth, el header es del cliente y el modo solo es apto para desarrollo.

**Alternativa descartada:** conceder `*` implícitamente cuando `scopes` está vacío. Es cómodo en demos y catastrófico cuando una key de solo lectura se copia a producción.

### 2. Entropía, pepper y throttle

- Con `admin.allow_insecure: false`, las keys de tenant deben tener ≥ 32 caracteres. En dev se permite una key corta para curl, pero strict nunca.
- `auth.key_pepper` / `AUTH_KEY_PEPPER` aplica HMAC-SHA256 a los hashes en memoria. Sin pepper se mantiene SHA-256 por compatibilidad; sigue sin ser reversible, pero es vulnerable a precomputación si el mapa se filtra. Cambiar la pepper re-hashea las keys en la recarga.
- 20 fallos de autenticación por IP y minuto devuelven 429 `too_many_authentication_failures` con `Retry-After`. Es estado local al proceso: no pretende ser un WAF distribuido, solo encarecer el brute force por réplica.
- El admin usa comparación en tiempo constante, su propio throttle y un audit record por intento.

### 3. Strict mode (`--strict` / `GATEWAY_ENV=prod`)

Se añade una segunda pasada de validación, ejecutada al arrancar **y en cada recarga** (`applyConfig`). No es un modo de ejecución distinto: es un contrato de configuración. Exige:

- `auth.enabled: true`; `admin.api_key` presente; `admin.allow_insecure: false`; `admin.pprof_enabled: false`.
- CORS sin `*`.
- `base_url` https y sin hosts locales/privados/loopback/link-local/metadata, salvo `allow_private: true`; los metadata endpoints se bloquean incluso con `allow_private`.
- `REDIS_URL` si rate limit o budget están activos, y `budget.require_shared_store: true` si hay budget.

Un fallo imprime todas las infracciones y no arranca (o rechaza la recarga conservando la config anterior). El manifiesto K8s y el perfil `prod` de Compose ya lo activan.

**Alternativa descartada:** un flag `production: true` dentro del YAML. El entorno es lo que cambia entre dev y prod; el archivo suele ser el mismo artefacto. Tenerlo en el proceso permite que un archivo olvidado no degrade el despliegue.

### 4. Validación de `base_url`

- Esquema `https` por defecto. `allow_private: true` permite `http` y direcciones privadas para un backend local de confianza (Ollama, vLLM); no permite metadata.
- Se bloquean `localhost`, `.localhost`, `.local`, loopback, privadas, link-local, unspecified y `169.254.169.254` / `100.100.100.200` / `fd00:ec2::254`.
- Esto es validación de configuración, no un sandbox de red: no hay pinning de DNS ni allowlist de redirects. Un operador malicioso con acceso al YAML sigue pudiendo apuntar a un host permitido. La defensa contra DNS rebinding requiere un resolver controlado y queda en R-23.

### 5. Admin: fail-closed, rotación y ETag

- Sin key y con `allow_insecure: false`, el proceso no arranca. Si el handler se cablea sin key por un bug, responde 503, no sirve el control plane.
- La admin key se rota **en caliente** (`SetAPIKey`/`SetRequireAuth` desde `applyConfig`). No hace falta reiniciar; con varias réplicas cada una aplica la rotación cuando recarga.
- `GET /admin/config` devuelve un `ETag`; `PATCH` acepta `If-Match` y responde 409 si la configuración cambió. Los PATCH se serializan para que el read-modify-apply sea significativo.
- Cada operación autenticada (éxito, fallo, bloqueo, reload, patch, conflicto) emite `admin_audit` con actor fingerprint, acción y detalle redactado.

## Consecuencias

**Positivas**

- Un error de configuración falla al arrancar en vez de abrir la API o multiplicar la cuota.
- El data plane puede añadir rutas sin que una ruta nueva sea pública por accidente.
- El admin es operable sin reiniciar y sin perder cambios concurrentes.
- La validación de `base_url` cubre el caso común de SSRF por configuración.

**Negativas / costes**

- Más fricción en dev: una key corta o `auth.enabled: false` no pasa strict. Es intencional; `config.dev.yaml` + `allow_insecure: true` sigue funcionando.
- Cambiar `GATEWAY_ENV` o `admin.pprof_enabled` exige reiniciar; no se puede “apagar strict” en caliente.
- El throttle de 401 y el audit son por proceso, no compartidos.
- La validación de `base_url` no sustituye a un egress firewall ni a un resolver controlado.

**Riesgos**

- Un operador puede desactivar strict y volver a los defaults. Mitigación: el manifiesto y el perfil prod lo activan, y la revisión de despliegue debe tratar `GATEWAY_ENV` como parte del contrato.
- La pepper perdida invalida todas las keys. Mitigación: gestionarla como secreto versionado y rotar keys+pepper juntas.

## Criterios de aceptación

- `GATEWAY_ENV=prod` con `config.dev.yaml` falla con una lista de infracciones y código de salida distinto de cero.
- Una key sin scopes, con scope desconocido, o de < 32 caracteres en producción no arranca.
- `GET /v1/models` con una key sin `models:read` devuelve 403 `insufficient_scope`; una ruta `/v1/*` no mapeada devuelve 403.
- Un PATCH con `If-Match` obsoleto devuelve 409 y no muta la config.
- Rotar `admin.api_key` + reload invalida la key vieja sin reiniciar.
- `base_url: http://127.0.0.1:11434` falla en strict salvo `allow_private: true`; `http://169.254.169.254` falla siempre.

## Enlaces

- [`../RUNBOOK.md`](../RUNBOOK.md) — procedimientos de strict, rotación, ETag y scopes.
- [`../../SECURITY.md`](../../SECURITY.md) — modelo de amenazas y controles.
- [`../ROADMAP.md`](../ROADMAP.md) — R-23/R-24/R-26 y lo que sigue pendiente.
