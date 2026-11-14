#!/usr/bin/env python3
"""Minimal OpenAI-compatible upstream for SDK conformance tests.

It is deliberately dependency-free (stdlib only) and deterministic. The
gateway is the component under test; this server only needs to behave like an
OpenAI backend for chat, streaming, tools, embeddings and /models.
"""

import argparse
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MODELS = ["gpt-4o", "gpt-4o-mini", "text-embedding-3-small"]
USAGE = {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):  # keep CI logs readable
        pass

    def _send_json(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _error(self, status, message, code):
        self._send_json(status, {"error": {"message": message, "type": "invalid_request_error", "code": code}})

    def _read_json(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length) if length else b"{}"
        return json.loads(raw or b"{}")

    def do_GET(self):
        if self.path.rstrip("/") == "/v1/models":
            self._send_json(200, {
                "object": "list",
                "data": [
                    {"id": model, "object": "model", "created": 1704067200, "owned_by": "fake"}
                    for model in MODELS
                ],
            })
            return
        self._error(404, "not found", "not_found")

    def do_POST(self):
        if self.headers.get("Authorization", "") != "Bearer test-upstream-key":
            self._error(401, "invalid upstream key", "invalid_api_key")
            return
        if self.path.rstrip("/") == "/v1/chat/completions":
            self._chat()
            return
        if self.path.rstrip("/") == "/v1/embeddings":
            self._embeddings()
            return
        self._error(404, "not found", "not_found")

    def _chat(self):
        req = self._read_json()
        model = req.get("model", "gpt-4o")
        if req.get("stream"):
            self._stream(model, req)
            return
        if req.get("tools") and req.get("tool_choice") != "none":
            message = {
                "role": "assistant",
                "content": None,
                "tool_calls": [{
                    "id": "call_fake_1",
                    "type": "function",
                    "function": {"name": "lookup", "arguments": "{\"q\":\"conformance\"}"},
                }],
            }
            finish = "tool_calls"
        else:
            text = _last_user_text(req)
            message = {"role": "assistant", "content": f"echo: {text}"}
            finish = "stop"
        self._send_json(200, {
            "id": "chatcmpl-fake-1",
            "object": "chat.completion",
            "created": 1704067200,
            "model": model,
            "choices": [{"index": 0, "message": message, "finish_reason": finish}],
            "usage": USAGE,
        })

    def _stream(self, model, req):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "keep-alive")
        self.end_headers()

        def chunk(delta, finish=None):
            return {
                "id": "chatcmpl-fake-stream",
                "object": "chat.completion.chunk",
                "created": 1704067200,
                "model": model,
                "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
            }

        frames = [
            chunk({"role": "assistant"}),
            chunk({"content": "hello "}),
            chunk({"content": "world"}),
            chunk({}, "stop"),
            {"id": "chatcmpl-fake-stream", "object": "chat.completion.chunk", "created": 1704067200,
             "model": model, "choices": [], "usage": USAGE},
        ]
        for frame in frames:
            self.wfile.write(f"data: {json.dumps(frame)}\n\n".encode())
            self.wfile.flush()
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()
        self.close_connection = True

    def _embeddings(self):
        req = self._read_json()
        inputs = req.get("input")
        if isinstance(inputs, str):
            inputs = [inputs]
        data = [
            {"object": "embedding", "index": i, "embedding": [0.1, 0.2, 0.3]}
            for i in range(len(inputs))
        ]
        self._send_json(200, {
            "object": "list",
            "data": data,
            "model": req.get("model", "text-embedding-3-small"),
            "usage": {"prompt_tokens": 2, "total_tokens": 2},
        })


def _last_user_text(req):
    for message in reversed(req.get("messages", [])):
        if message.get("role") == "user":
            content = message.get("content", "")
            if isinstance(content, list):
                return " ".join(part.get("text", "") for part in content if isinstance(part, dict))
            return str(content)
    return ""


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=18081)
    args = parser.parse_args()
    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    print(f"fake upstream listening on 127.0.0.1:{args.port}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
