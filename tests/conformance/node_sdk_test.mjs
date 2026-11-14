import OpenAI from "openai";

const BASE_URL = "http://127.0.0.1:18080/v1";
const API_KEY = "conformance-tenant-key-0123456789abcdef0123456789abcdef";
const MODEL = "gpt-4o";

function check(condition, message) {
  if (!condition) throw new Error(message);
}

const client = new OpenAI({ baseURL: BASE_URL, apiKey: API_KEY, maxRetries: 0, timeout: 10_000 });

const completion = await client.chat.completions.create({
  model: MODEL,
  messages: [{ role: "user", content: "node conformance" }],
});
check(completion.id === "chatcmpl-fake-1", `id = ${completion.id}`);
check(completion.choices[0].message.content === "echo: node conformance", "content mismatch");
check(completion.usage.total_tokens === 8, `usage = ${JSON.stringify(completion.usage)}`);
console.log("node: chat completion OK");

const stream = await client.chat.completions.create({
  model: MODEL,
  messages: [{ role: "user", content: "stream" }],
  stream: true,
  stream_options: { include_usage: true },
});
let text = "";
let usage = null;
for await (const chunk of stream) {
  if (chunk.choices?.length) text += chunk.choices[0].delta.content ?? "";
  if (chunk.usage) usage = chunk.usage;
}
check(text === "hello world", `stream text = ${JSON.stringify(text)}`);
check(usage?.total_tokens === 8, `stream usage = ${JSON.stringify(usage)}`);
console.log("node: streaming OK");

const toolCompletion = await client.chat.completions.create({
  model: MODEL,
  messages: [{ role: "user", content: "use a tool" }],
  tools: [{
    type: "function",
    function: { name: "lookup", parameters: { type: "object", properties: {} } },
  }],
  tool_choice: "auto",
});
check(toolCompletion.choices[0].message.tool_calls?.[0]?.function.name === "lookup", "tool call mismatch");
console.log("node: tools OK");

const embeddings = await client.embeddings.create({ model: "text-embedding-3-small", input: ["a", "b"] });
check(embeddings.data.length === 2, `embedding count = ${embeddings.data.length}`);
check(embeddings.data[0].embedding.length === 3, "embedding dimensions");
check(embeddings.usage.prompt_tokens === 2, `embedding usage = ${JSON.stringify(embeddings.usage)}`);
console.log("node: embeddings OK");

const models = await client.models.list();
check(models.data.some((model) => model.id === "gpt-4o"), `models = ${JSON.stringify(models.data)}`);
console.log("node: models OK");

try {
  await client.chat.completions.create({
    model: "does-not-exist",
    messages: [{ role: "user", content: "x" }],
  });
  throw new Error("unknown model did not fail");
} catch (error) {
  check(error?.status === 404, `expected 404, got ${error?.status}`);
  console.log("node: 404 mapping OK");
}

const idem = crypto.randomUUID();
const request = {
  model: MODEL,
  messages: [{ role: "user", content: "idempotent" }],
};
const headers = {
  Authorization: `Bearer ${API_KEY}`,
  "Content-Type": "application/json",
  "Idempotency-Key": idem,
};
const first = await fetch(`${BASE_URL}/chat/completions`, { method: "POST", headers, body: JSON.stringify(request) });
const firstBody = await first.text();
const second = await fetch(`${BASE_URL}/chat/completions`, { method: "POST", headers, body: JSON.stringify(request) });
const secondBody = await second.text();
check(first.status === 200 && second.status === 200, `idempotency statuses ${first.status}/${second.status}`);
check(firstBody === secondBody, "idempotent response changed");
check(second.headers.get("idempotent-replayed") === "true", "missing Idempotent-Replayed header");
console.log("node: idempotency OK");

console.log("node SDK conformance: OK");
