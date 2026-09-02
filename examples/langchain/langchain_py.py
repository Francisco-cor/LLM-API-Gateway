"""LangChain via gateway — ChatOpenAI base_url swap"""
from langchain_openai import ChatOpenAI
from langchain_core.messages import HumanMessage, SystemMessage

llm = ChatOpenAI(
    base_url="http://localhost:8080/v1",
    api_key="local-dev",
    model="claude-sonnet-4-6",
    temperature=0.2,
    max_tokens=64,
)

resp = llm.invoke([
    SystemMessage(content="You are concise."),
    HumanMessage(content="What is a token bucket?"),
])
print(resp.content)
print(resp.response_metadata)
# streaming
for chunk in llm.stream([HumanMessage(content="Count 1 to 5")]):
    print(chunk.content, end="", flush=True)
print()
