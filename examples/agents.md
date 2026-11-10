# Coding agents on wend

A coding agent calls tools, so give it the `tools` profile: only models that call
tools well are taken, and the next one steps in when one fails.

## Claude Code

```
export ANTHROPIC_BASE_URL=http://localhost:8888
export ANTHROPIC_AUTH_TOKEN=wend
export ANTHROPIC_MODEL=tools
claude
```

On Windows PowerShell: `$env:ANTHROPIC_BASE_URL = "http://localhost:8888"`, and so on.
Claude Code also asks for `claude-*` models of its own for small jobs; wend serves
those with the profile in `ROUTER_ANTHROPIC_PROFILE` (`auto` unless you set it).

## Flint

```
FLINT_PROVIDER=openai
FLINT_BASE_URL=http://localhost:8888/v1
OPENAI_API_KEY=wend
flint --model tools
```

## Any tool that takes an OpenAI address

| Setting | Value |
|---|---|
| Base URL | `http://localhost:8888/v1` |
| API key | any text (the `ROUTER_TOKEN` value, if you set one) |
| Model | `tools` for agents, `auto` for chat |

The dashboard's **Connect your agent** section writes these out for the profile you
pick, with a Test button.
