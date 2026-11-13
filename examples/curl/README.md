# curl examples

```bash
chmod +x *.sh
./chat.sh          # non-stream chat
./stream.sh        # SSE streaming
./embeddings.sh
./models.sh        # models + health + metrics + admin probe
```

Environment:

| Variable | Default | Meaning |
|---|---|---|
| `GATEWAY_URL` | `http://localhost:8080` | Gateway base URL |
| `GATEWAY_API_KEY` | `local-dev` | Tenant key when auth is enabled |
| `MODEL` | script-specific | Model name; must belong to a configured provider |
| `ADMIN_API_KEY` | empty | Admin key for the probe in `models.sh` |

`models.sh` reads `GET /admin/config` and prints `.Admin.APIKey` (Go field names, not YAML keys). The admin API binds to `127.0.0.1` by default; in Docker Compose it is not reachable from the host unless you deliberately set `admin.bind_address`. The admin response is a JSON encoding of the Go config struct, so durations are integer nanoseconds and secret fields are `***`.

Requires `jq` and `curl`. The scripts do not require auth when `auth.enabled: false` in the active config; the `Authorization` header is harmless then.
