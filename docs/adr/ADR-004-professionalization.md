# ADR-004: Profesionalización — control plane, ledger y límites

**Status:** Proposed (dirección aceptada para el roadmap; implementación pendiente)
**Date:** 2026-11-13 · **Deciders:** maintainer
**Supersedes:** nada. Complementa ADR-001, ADR-002 y ADR-003.
**Related:** [`../ROADMAP.md`](../ROADMAP.md), [`ADR-005-fail-closed-auth-and-strict-mode.md`](ADR-005-fail-closed-auth-and-strict-mode.md) (implementa la parte de auth fail-closed y strict de la decisión 4).

## Contexto

El gateway actual demuestra el data plane: traducción OpenAI, streaming, fallback, circuit breaker, rate limit, cache y budget. Pero el estado de control vive en un YAML y en memoria; el uso se guarda en contadores mensuales sin ledger; el routing no conoce capacidades; y el fallback de Redis es silencioso. Eso es suficiente para una demo y para una entrevista, no para operar un servicio multi-tenant con dinero de por medio.

La pregunta de este ADR no es “qué features añadir”, sino **dónde vive el estado, qué se puede degradar y qué no**. Cada decisión busca que el data plane siga siendo rápido y que el control plane sea auditable.

## Decisiones

### 1. Postgres como control plane; el data plane no consulta la DB en el hot path

Postgres guarda tenants, keys, proveedores, modelos, precios, rutas, budgets, ledger y auditoría. El gateway recibe un **snapshot de configuración** (versión + ETag) y lo mantiene en memoria. Las peticiones de chat/embeddings resuelven contra ese snapshot; no hacen `SELECT` por request.

- Sincronización inicial al arrancar y polling incremental cada pocos segundos (o `LISTEN/NOTIFY` como optimización).
- Si el control plane no está disponible, el data plane sigue sirviendo con el último snapshot conocido y expone una métrica de antigüedad. No se cae la API por un reinicio de Postgres.
- El admin actual deja de ser la fuente de verdad. `PATCH /admin/config` se convierte en una operación de control plane (persistida y auditada), no en un cambio efímero del proceso.

**Alternativas descartadas:** mantener YAML como fuente de verdad (no hay CRUD ni auditoría razonable); leer Postgres por request (latencia y acoplamiento inaceptables); etcd (más infraestructura para un problema que Postgres ya resuelve).

### 2. Ledger append-only y escritura asíncrona

Cada petición aceptada emite un `usage_event` con tenant, key, proveedor, modelo, tokens, coste, estado y latencia. El data plane escribe en un buffer acotado; un worker inserta por lotes con idempotencia (`event_id`). El hot path nunca espera a Postgres.

- Si el buffer se llena: se descarta el evento, se incrementa `gateway_usage_events_dropped_total` y se alerta. **No se bloquea la respuesta al cliente.**
- La contabilidad de cuotas se hace contra rollups del ledger; el contador de budget en Redis sigue existiendo como mecanismo de decisión rápida, pero el ledger es la verdad para reconciliar.
- Reconciliación diaria: suma del ledger vs contadores; desvío > 0,5 % genera alerta.

**Alternativa descartada:** escribir el evento de forma síncrona antes de responder. Convierte una caída de Postgres en una caída del gateway y añade latencia a cada token.

### 3. Capability registry y fallback por capacidad

Los modelos declaran capacidades (`chat`, `stream`, `tools`, `vision`, `json_schema`, `embeddings`, `context_window`, etc.). El routing y el fallback filtran candidatos por las capacidades que la petición requiere.

- Una petición con `tools` nunca cae a un modelo sin tools; si no hay candidato, se devuelve un 400/503 explícito antes de llamar al proveedor.
- Las capacidades son datos versionados, no `if strings.HasPrefix(model, "gpt-")`.
- El fallback registra qué capacidad motivó la elección para poder depurar.

**Alternativa descartada:** mantener listas hardcodeadas por proveedor. Se desincroniza en cuanto un proveedor cambia un modelo.

### 4. Fail-closed en control y dinero; fail-open explícito y medido en caché

Política por dependencia:

| Dependencia | Si falla | Razón |
|---|---|---|
| Admin API key ausente | El proceso no arranca | El control plane puede reescribir configuración y exponer pprof |
| Budget / cuota | 503 `budget_unavailable` o 429 `insufficient_quota` | Un fallo de Redis no puede convertirse en gasto ilimitado |
| Rate limit distribuido (Redis) | Fallback local **con métrica y log de advertencia**; en modo estricto, 503 | Preferimos disponibilidad por defecto, pero el operador debe poder elegir corrección global |
| Caché | Se bypassa (miss) | La caché es una optimización; nunca debe impedir una respuesta |
| Control plane (Postgres) | Último snapshot conocido + métrica de antigüedad | Ya hay un snapshot válido en memoria |
| Ingesta del ledger | Buffer + descarte medido | No se bloquea el chat por telemetría |

El fallback silencioso actual del rate limit se mantiene por compatibilidad, pero pasa a ser **explícito**: log `warn` una vez por transición, métrica de backend y alerta. Se añadirá un modo `strict` para tenants de pago.

### 5. Contrato primero y proveedor genérico

`api/openapi.yaml` es la referencia pública. Añadir un proveedor OpenAI-compatible (Groq, DeepSeek, OpenRouter, Ollama, vLLM) debe ser configuración + un adaptador fino, no una rama en `main.go`. Azure/Bedrock/Vertex son adaptadores con firma propia, pero comparten el mismo contrato de entrada/salida y la misma suite de conformidad.

### 6. Migraciones y compatibilidad

- Un solo directorio `migrations/`, checksum y bloqueo; el control plane no arranca con el esquema desactualizado.
- Cambios de esquema compatibles hacia atrás durante una release (expand/contract).
- El snapshot de configuración lleva versión; una réplica con versión vieja sigue sirviendo y reporta antigüedad.

## Consecuencias

**Positivas**

- El data plane conserva su latencia y su independencia de la DB.
- Tenants, keys, rutas y precios se pueden operar sin editar YAML ni reiniciar.
- El uso es auditable y reconcilable; las cuotas se pueden defender ante finanzas.
- El fallback deja de ser una caja negra: se sabe qué se degradó y por qué.

**Negativas / costes**

- Dos planos que operar: Postgres + gateway + Redis. Más superficie, más backups, más migraciones.
- La consistencia entre réplicas es eventual (≤ 5 s objetivo); hay que diseñar para ello.
- El ledger asíncrono puede perder eventos bajo presión; la métrica de descarte es obligatoria.
- Añadir un proveedor deja de ser “un archivo” y pasa a ser “un archivo + capacidades + fixtures”.

**Riesgos**

- Complejidad prematura si se implementa todo antes de tener usuarios. Mitigación: fases del ROADMAP y no empezar por la UI.
- Postgres se convierte en punto único si el control plane se diseña mal. Mitigación: el data plane nunca depende de él en el hot path; backups y réplica de lectura.
- El ledger puede crecer sin control. Mitigación: particiones mensuales y retención/exportación desde el día uno.

## Plan de migración desde el estado actual

1. Importar el YAML actual a Postgres con un comando `gatewayctl import --config config.yaml` (idempotente).
2. Levantar el control plane en paralelo; el gateway sigue leyendo el snapshot del archivo hasta que la importación esté validada.
3. Cambiar la fuente del snapshot a Postgres con feature flag `control_plane.mode: postgres`.
4. Retirar el file watcher como fuente de verdad; conservarlo solo como modo standalone/dev.
5. Activar ledger primero en modo observación (escribe pero no decide cuotas), luego en enforcement.

## Criterios de aceptación de la dirección

- Un tenant y una key creados por API funcionan sin reiniciar el gateway.
- Una revocación se propaga a todas las réplicas en ≤ 5 s.
- El desvío ledger vs budget es < 0,5 % durante 7 días.
- Una petición con `tools` no se enruta a un modelo sin tools.
- Un fallo de Redis no produce gasto ilimitado ni caída del chat.
- Un fallo de Postgres no impide servir con el último snapshot.

## Enlaces

- [`../ROADMAP.md`](../ROADMAP.md) — fases, backlog y esquema SQL.
- [`../RUNBOOK.md`](../RUNBOOK.md) — qué hacer cuando Redis/Postgres/proveedor fallan.
- [`../SECURITY.md`](../../SECURITY.md) — modelo de amenazas y controles.
