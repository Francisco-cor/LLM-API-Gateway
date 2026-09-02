#!/usr/bin/env python3
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="local-dev")

resp = client.embeddings.create(model="text-embedding-3-small", input="hello world")
print(resp.data[0].embedding[:5], "...", len(resp.data[0].embedding))
# array input
resp2 = client.embeddings.create(model="text-embedding-3-small", input=["hello", "world"])
print([len(d.embedding) for d in resp2.data])
