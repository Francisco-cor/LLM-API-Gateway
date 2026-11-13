# Política de seguridad

## Versiones soportadas

| Versión | Soportada | Notas |
|---|---|---|
| `1.1.x` | Sí | Línea actual (`v1.1.0`). |
| `1.0.x` | Best effort | Se pueden backportear fixes pequeños y de bajo riesgo. |
| `< 1.0` | No | Historial pre-release; actualiza. |

Solo el último parche de una minor soportada recibe fixes de seguridad. `main` es desarrollo; un fix en `main` no está publicado hasta que existe una etiqueta.

## Cómo reportar una vulnerabilidad

**No abras un issue público para un problema de seguridad.**

Usa el reporte privado de GitHub:

1. Pestaña **Security** del repositorio → **Advisories** → **Report a vulnerability**.
2. O abre directamente un **private security advisory** desde la página del repositorio. GitHub mantiene la conversación privada hasta que se publica.

Incluye, si puedes:

- versión o commit afectado (`cat VERSION`, `git rev-parse HEAD`);
- una reproducción mínima (config sin secretos, petición, observado vs esperado);
- impacto (confidencialidad, integridad, disponibilidad, coste);
- si el problema es alcanzable con una API key de tenant, con acceso admin o solo con acceso local.

No hay dirección de email de seguridad ni bug bounty. No envíes reportes a direcciones personales. Si el reporte privado de GitHub no está disponible, abre un issue público mínimo pidiendo al maintainer habilitar un canal privado, sin revelar detalles.

**Qué esperar:** acuse de recibo en el advisory, decisión de triage y coordinación de la divulgación. No se promete un SLA fijo; es un proyecto de un solo maintainer. Da tiempo a un fix antes de publicar. Se dará crédito en el advisory salvo que pidas lo contrario.

## Alcance

Dentro del alcance:

- la superficie HTTP del gateway (`/v1/*`, `/health*`, `/metrics`);
- la API de admin y las rutas de pprof (`/admin/*`, `/debug/pprof/*`);
- autenticación, autorización, aislamiento de tenants, rate limit, budget y caché;
- adaptadores de proveedor y capa de traducción, incluido el manejo de secretos y las URLs relevantes para SSRF;
- la imagen de contenedor, los manifiestos de Kubernetes y la configuración por defecto del repositorio;
- dependencias, cuando la vulnerabilidad sea explotable a través de este proyecto.

Fuera del alcance:

- vulnerabilidades de OpenAI, Anthropic, Gemini o cualquier proveedor upstream;
- DoS volumétrico, salvo que explote un recurso no acotado específico de este código;
- calidad de las respuestas del modelo, alucinaciones o prompt injection que solo afecte a la respuesta (repórtalo si cruza una frontera de confianza del gateway);
- mala configuración documentada explícitamente como insegura (por ejemplo `admin.allow_insecure: true` en una interfaz pública);
- hallazgos que requieran un host comprometido o acceso de cluster-admin.

## Modelo de amenazas (resumen)

### Activos

- Claves de API de proveedores (dinero y acceso a la cuenta).
- Claves de tenant y sus scopes.
- Prompts y respuestas (posiblemente personales o confidenciales).
- Estado de uso/budget (integridad financiera).
- Control plane de admin (puede reescribir configuración y exponer pprof).

### Fronteras de confianza

```text
cliente tenant ──Bearer key──> gateway :8080 ──provider key──> upstream
                                      │
operador ──admin key──> admin :8081 ──┘ (mismo proceso)
```

### Amenazas y postura actual

| Amenaza | Control actual | Brecha / pendiente |
|---|---|---|
| **Admin API expuesta** | El arranque es fail-closed: con `admin.allow_insecure=false` y sin key, el proceso no arranca. Comparación de key en tiempo constante. Bind por defecto a `127.0.0.1`. | La admin key no se puede rotar sin reiniciar; no hay identidad por operador ni audit log; no hay mTLS. Roadmap R-09, R-26. |
| **pprof filtra secretos/prompts** | pprof es **opt-in** (`admin.pprof_enabled`, false por defecto) y vive solo en el mux del admin; no hay listener `:6060`. | Un heap dump contiene claves y prompts. Trátalo como secreto, restringe la red del admin y deshabilita pprof después. |
| **SSRF vía `base_url`** | `base_url` lo controla el operador, no el tenant; los clientes tienen timeout y la respuesta upstream está acotada a 8 MiB. | No hay allowlist de esquema/host, ni bloqueo de IP privadas/loopback/link-local; se siguen redirects HTTP por defecto; no se cubre DNS rebinding. Roadmap R-23. |
| **Aislamiento de tenants** | El middleware deriva el tenant de la key y **sobrescribe** `X-Tenant-ID`; las claves de caché se salan con SHA-256 del valor de Authorization; los namespaces semánticos incluyen identidad y forma de respuesta; se aplican scopes y expiración por endpoint. Hay tests unitarios de aislamiento por identidad. | Con `auth.enabled=false`, `X-Tenant-ID` lo controla el cliente (solo modo dev). No hay suite e2e de aislamiento por HTTP; no hay CRUD ni propagación de revocación (config estática). Roadmap R-06/R-07. |
| **Exposición de secretos en logs** | `slog` redacta atributos llamados `authorization`, `x-api-key`, `api_key`, `api-key`, `key` y valores que parecen `Bearer ...` o `sk-...`; el admin redacta claves de proveedor/admin/Redis/auth como `***`. Gemini usa `x-goog-api-key`, no query string. | La redacción es por nombre/heurística; los prompts no se registran por defecto, pero pueden aparecer en errores. Los mensajes de error del proveedor (acotados a 4 KiB) se devuelven al cliente. No hay secret manager. Roadmap R-24/R-25. |
| **Fuga de prompts/contenido vía caché** | Las entradas están aisladas por identidad y campos que moldean la respuesta; TTL acotado; solo 200 no-stream; los streams no se cachean. | El contenido se guarda sin cifrar en memoria/Redis; Redis es otra frontera de confianza; la caché semántica es local al proceso. No hay clasificación de PII. |
| **Fuerza bruta de credenciales** | Las API keys son de alta entropía y se comparan por hash; una key inexistente usa una comparación dummy en tiempo constante. El rate limit va por identidad hasheada. | No hay alerta de fallos de autenticación por IP. |
| **Bypass de rate limit con Redis caído** | El bucket local mantiene protección por proceso. | El límite global se multiplica por el número de réplicas; el flag `degraded` no tiene métrica. Roadmap R-27. |
| **Bypass/overspend de budget** | Las reservas son atómicas (Redis Lua o mutex local); el budget falla cerrado (`503`) cuando el Redis configurado no está disponible; los precios desconocidos se pueden rechazar. | No hay ledger ni reconciliación; no hay cuotas por tenant en UI; el modo local es por réplica. Roadmap R-10/R-11. |
| **Smuggling / cuerpos gigantes** | `MaxBytesReader` de 1 MiB en chat/embeddings y PATCH admin; campos JSON desconocidos rechazados; un solo valor JSON por cuerpo; respuestas upstream acotadas. | No hay fuzzing dedicado de la capa de traducción en CI. |
| **Métricas sin autenticar** | `/metrics` se excluye intencionadamente de la auth de tenant. | Expone tráfico, coste, proveedor y etiquetas adyacentes al tenant. Restringe por red o añade auth. Roadmap R-26. |
| **Supply chain** | `govulncheck` y `golangci-lint` (incluye `gosec`) corren en CI; las imágenes base van por tag, no por digest. | No hay SBOM, firma de imagen ni pin por digest. |
| **Transporte** | Gateway↔proveedor usa HTTPS por defecto; la verificación TLS es la de Go. | No hay mTLS con proveedores; Redis puede ir en claro en el perfil compose; no hay terminación TLS en el repositorio. |

### No objetivos explícitos hoy

- El gateway **no** es una frontera de cumplimiento (HIPAA, PCI, SOC 2). No clasifica ni redacta PII.
- No defiende contra un operador malicioso con acceso admin: la API de admin puede cambiar rutas y URLs de proveedor por diseño.
- No sandboxea la salida del modelo ni la ejecución de tools; el cliente ejecuta las tools.

## Checklist de hardening para operadores

- [ ] Define `ADMIN_API_KEY` con un valor aleatorio de ≥ 32 bytes; nunca uses `admin.allow_insecure: true` fuera de localhost.
- [ ] Mantén `admin.bind_address: 127.0.0.1` salvo que tengas NetworkPolicy/ACL; no expongas `:8081` por un Ingress público.
- [ ] Deja `admin.pprof_enabled: false`; actívalo solo durante un incidente, descarga los dumps de forma segura, bórralos y desactívalo.
- [ ] Exige auth (`auth.enabled: true`) en todo despliegue no local; limita los scopes a `chat:write`, `embeddings:write`, `models:read`.
- [ ] Pon Redis en una red privada con auth/TLS; no lo reutilices para datos no relacionados.
- [ ] Restringe `/metrics` a la red de monitorización o añade un proxy autenticado.
- [ ] Activa `rate_limit.budget.enabled: true` con un límite mensual real; monitoriza `gateway_cost_usd_total`.
- [ ] Usa `readOnlyRootFilesystem`, `runAsNonRoot` y capabilities eliminadas (los manifiestos K8s ya lo hacen).
- [ ] Fija digests de imagen y escanéalos (`docker scout`, `trivy`) en tu propio pipeline.
- [ ] Rota las keys de tenant con solape; recuerda que la admin key requiere reinicio.
- [ ] Haz backup de `config.yaml`/secretos y prueba la restauración; hasta que exista Postgres, el YAML es el control plane.

## Controles actuales vs pendientes (vista rápida)

| Control | Estado |
|---|---|
| Auth Bearer de tenant, scopes, expiración | Implementado |
| Admin fail-closed + comparación en tiempo constante | Implementado |
| Bind del admin a loopback por defecto | Implementado |
| pprof opt-in solo dentro del admin | Implementado |
| Redacción de secretos en logs/salida admin | Implementado (heurístico) |
| Límites de tamaño de petición/respuesta | Implementado |
| CORS allowlist, cabeceras de seguridad | Implementado |
| Budget fail-closed con Redis | Implementado |
| SSRF allowlist / bloqueo de IP privadas | **Pendiente** (R-23) |
| Secret manager + rotación | **Pendiente** (R-24) |
| Redacción de PII / política de logs de prompts | **Pendiente** (R-25) |
| Audit log de admin + identidad de operador | **Pendiente** (R-09) |
| Autenticación de métricas | **Pendiente** (R-26) |
| SBOM + firma de imagen | **Pendiente** |
| Ledger / reconciliación | **Pendiente** (R-10/R-12) |

Si estás evaluando este proyecto para producción, trata los pendientes como bloqueantes, no como mejoras opcionales.
