import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://localhost:8080/v1",
  apiKey: "local-dev",
});

const resp = await client.chat.completions.create({
  model: "claude-sonnet-4-6",
  messages: [
    { role: "system", content: "You are concise." },
    { role: "user", content: "What is a token bucket?" },
  ],
  max_tokens: 64,
});
console.log(resp.choices[0].message.content);
console.log(resp.usage);
