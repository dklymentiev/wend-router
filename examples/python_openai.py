# The openai package pointed at wend: pip install openai
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8888/v1", api_key="wend")  # any key works

reply = client.chat.completions.create(
    model="auto",
    messages=[{"role": "user", "content": "Name three rivers in one line."}],
)
print(reply.model, "said:", reply.choices[0].message.content)

# streamed: the text arrives as the model writes it
stream = client.chat.completions.create(
    model="fast",
    stream=True,
    messages=[{"role": "user", "content": "Count from 1 to 5."}],
)
for chunk in stream:
    if chunk.choices and chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="", flush=True)
print()
