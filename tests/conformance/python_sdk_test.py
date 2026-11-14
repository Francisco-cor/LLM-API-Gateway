#!/usr/bin/env python3
"""OpenAI Python SDK conformance against the real gateway + fake upstream."""

import sys
import uuid

from openai import NotFoundError, OpenAI

BASE_URL = "http://127.0.0.1:18080/v1"
API_KEY = "conformance-tenant-key-0123456789abcdef0123456789abcdef"
MODEL = "gpt-4o"


def check(condition, message):
    if not condition:
        raise AssertionError(message)


def main():
    client = OpenAI(base_url=BASE_URL, api_key=API_KEY, max_retries=0, timeout=10)

    completion = client.chat.completions.create(
        model=MODEL,
        messages=[{"role": "user", "content": "python conformance"}],
    )
    check(completion.id == "chatcmpl-fake-1", f"id = {completion.id}")
    check(completion.object == "chat.completion", f"object = {completion.object}")
    check(completion.choices[0].message.content == "echo: python conformance", "content mismatch")
    check(completion.usage.total_tokens == 8, f"usage = {completion.usage}")
    print("python: chat completion OK")

    stream = client.chat.completions.create(
        model=MODEL,
        messages=[{"role": "user", "content": "stream"}],
        stream=True,
        stream_options={"include_usage": True},
    )
    text = ""
    usage = None
    for chunk in stream:
        if chunk.choices:
            text += chunk.choices[0].delta.content or ""
        if chunk.usage:
            usage = chunk.usage
    check(text == "hello world", f"stream text = {text!r}")
    check(usage is not None and usage.total_tokens == 8, f"stream usage = {usage}")
    print("python: streaming OK")

    tool_completion = client.chat.completions.create(
        model=MODEL,
        messages=[{"role": "user", "content": "use a tool"}],
        tools=[{
            "type": "function",
            "function": {"name": "lookup", "parameters": {"type": "object", "properties": {}}},
        }],
        tool_choice="auto",
    )
    calls = tool_completion.choices[0].message.tool_calls
    check(calls is not None and calls[0].function.name == "lookup", f"tool calls = {calls}")
    print("python: tools OK")

    embeddings = client.embeddings.create(model="text-embedding-3-small", input=["a", "b"])
    check(len(embeddings.data) == 2, f"embedding count = {len(embeddings.data)}")
    check(len(embeddings.data[0].embedding) == 3, "embedding dimensions")
    check(embeddings.usage.prompt_tokens == 2, f"embedding usage = {embeddings.usage}")
    print("python: embeddings OK")

    models = client.models.list()
    ids = [model.id for model in models.data]
    check("gpt-4o" in ids, f"models = {ids}")
    print("python: models OK")

    try:
        client.chat.completions.create(model="does-not-exist", messages=[{"role": "user", "content": "x"}])
    except NotFoundError:
        print("python: 404 mapping OK")
    else:
        raise AssertionError("unknown model did not raise NotFoundError")

    idem = str(uuid.uuid4())
    first = client.chat.completions.with_raw_response.create(
        model=MODEL,
        messages=[{"role": "user", "content": "idempotent"}],
        extra_headers={"Idempotency-Key": idem},
    )
    second = client.chat.completions.with_raw_response.create(
        model=MODEL,
        messages=[{"role": "user", "content": "idempotent"}],
        extra_headers={"Idempotency-Key": idem},
    )
    check(first.parse().id == second.parse().id, "idempotent response changed")
    check(second.headers.get("idempotent-replayed") == "true", "missing Idempotent-Replayed header")
    print("python: idempotency OK")

    print("python SDK conformance: OK")


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:  # noqa: BLE001 - conformance harness prints the failure
        print(f"python SDK conformance failed: {exc}", file=sys.stderr)
        raise
