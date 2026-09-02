# curl examples

```bash
chmod +x *.sh
./chat.sh          # non-stream
./stream.sh        # SSE streaming
./embeddings.sh
./models.sh        # health + metrics
```
Env: `GATEWAY_URL` `GATEWAY_API_KEY` `MODEL` `ADMIN_API_KEY`.

Requires `jq` for pretty. Use `GATEWAY_API_KEY=local-dev` if auth disabled.
