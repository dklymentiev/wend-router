# Examples

Start wend first (see the main README); every example talks to
`http://localhost:8888` and works with no provider key, through the providers that
need none.

| File | What it shows | Run |
|---|---|---|
| [curl.sh](curl.sh) | a plain request and a streamed one | `sh curl.sh` |
| [python_openai.py](python_openai.py) | the `openai` package, plain and streamed | `pip install openai`, `python python_openai.py` |
| [python_anthropic.py](python_anthropic.py) | the `anthropic` package against the Messages API | `pip install anthropic`, `python python_anthropic.py` |
| [node.mjs](node.mjs) | Node's own `fetch`, no packages | `node node.mjs` |
| [agents.md](agents.md) | settings for coding agents: Claude Code, Flint, and any tool that takes an OpenAI address | |

The model is a profile (`auto`, `fast`, `tools`, `vision`, `no-think`, `private`, or
one you make on the dashboard) or a concrete `provider/model`. The answer says which
model served it.
