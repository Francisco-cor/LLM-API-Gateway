#!/usr/bin/env python3
"""openai-python via gateway — works with openai>=1.0"""
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="local-dev",  # any if auth disabled, or tenant key
)

resp = client.chat.completions.create(
    model="claude-sonnet-4-6",
    messages=[
        {"role": "system", "content": "You are concise."},
        {"role": "user", "content": "What is a token bucket?"},
    ],
    max_tokens=64,
    temperature=0.2,
)
print(resp.choices[0].message.content)
print(resp.usage)
print("provider:", resp.model)
