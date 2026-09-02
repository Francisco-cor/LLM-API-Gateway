#!/usr/bin/env python3
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="local-dev")

stream = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "Count 1 to 5"}],
    stream=True,
)
for chunk in stream:
    delta = chunk.choices[0].delta.content or ""
    print(delta, end="", flush=True)
print("\n[DONE]")
