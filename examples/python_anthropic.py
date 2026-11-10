# The anthropic package pointed at wend: pip install anthropic
# wend speaks the Messages API at /v1/messages; the base URL has no /v1.
from anthropic import Anthropic

client = Anthropic(base_url="http://localhost:8888", api_key="wend")  # any key works

# a profile name as the model; a claude-* name is served by the "auto" profile
reply = client.messages.create(
    model="auto",
    max_tokens=200,
    messages=[{"role": "user", "content": "Name three rivers in one line."}],
)
print(reply.model, "said:", reply.content[0].text)
