# Wend router: getting started

Wend router gives your tools one address for language models. Behind it are many
providers. When one model fails, runs out of quota or stalls, the next one answers,
so your tool never has to deal with that.

## 1. Check it works

The dot at the top says **Running** and how many models are up. A few providers need
no key, so models are up from the first minute.

## 2. Connect your tool

Under **Connect your agent**, copy the settings into your tool in place of OpenAI:

| Setting | Value |
|---|---|
| Base URL | this router's address with `/v1`, for example `http://127.0.0.1:8888/v1` |
| API key | any text; the router does not check it |
| Model | a profile such as `auto`, or one model |

Press **Test**: a short request goes through the router and the answer shows below.
Tabs give ready text for Flint, Python and curl.

Tools that speak the Anthropic API (Claude Code, for one) use the address without
`/v1` as `ANTHROPIC_BASE_URL`.

Looking for a coding agent? [Flint](https://flintagent.dev) is an open-source one that
works with Wend router out of the box: pick the Flint tab in Connect.

## 3. Choose what your tool asks for

The **Model** setting decides which models may answer:

- **A profile** (`auto`, `fast`, `tools`, `vision`, `no-think`, `private`, or your own):
  only the models of that profile, the router picks and switches between them.
- **provider/model** (`groq/openai/gpt-oss-120b`): exactly that model at that provider.
  No switching; if it cannot answer, your tool gets an error at once.
- **A model name** (`gpt-oss-120b`): that model at whichever provider serves it.

A request never goes to a model outside what it asked for. If none of them can answer,
the tool gets a clear error within seconds instead of waiting.

| Profile | Takes |
|---|---|
| `auto` | every model, strongest first |
| `fast` | every model, quickest to start first |
| `tools` | models that call tools: use it for coding agents |
| `vision` | models that read images |
| `no-think` | models without a reasoning phase, quickest first |
| `private` | only providers that do not train on prompts |

## 4. Add providers

More providers mean more models and more room before limits. Under **Providers**,
press **Key** next to a provider, open its sign-up link, create a free key and paste it.
Keys are stored encrypted on this machine.

## 5. Make your own profile

**New profile**: give it a name, choose what models must be able to do (tools,
vision), how to order them, and which providers. The list shows which models it takes
right now. Your tool then sends that name as the model.

## More

- **Pro** (top right) shows every model with its speed and health, the filter that
  hides names from models, and the Pipe that shows each request as it passes.
  Each Pro tab has its own help.
- Everything the router keeps is in files next to the program: keys (`keys.dat`,
  encrypted), profiles (`profiles.json`), state (`router-state.json`).

## For an agent setting this up

Everything on this page has an HTTP API on the same address. Changes are accepted
only from this machine and only with the header `X-Router-UI: 1`.

```
# is it up, how many models
curl -s http://127.0.0.1:8888/health

# models it can use now
curl -s http://127.0.0.1:8888/v1/models

# a request through a profile
curl -s http://127.0.0.1:8888/v1/chat/completions -H "Content-Type: application/json" \
  -d '{"model": "auto", "messages": [{"role": "user", "content": "hello"}]}'

# profiles: list, create or replace, delete
curl -s http://127.0.0.1:8888/api/profiles
curl -s -X PUT http://127.0.0.1:8888/api/profiles/coder -H "X-Router-UI: 1" \
  -d '{"require": ["tools"], "sort": "fastest"}'
curl -s -X DELETE http://127.0.0.1:8888/api/profiles/coder -H "X-Router-UI: 1"

# which models a profile would take, before saving it
curl -s -X POST http://127.0.0.1:8888/api/preview -d '{"require": ["tools"]}'
```

Profile fields: `require` and `exclude` (`tools`, `vision`, `reasoning`), `models`
(name patterns such as `*gpt-oss*`), `providers`, `exclude_providers`, `sort`
(`strongest`, `fastest`, `cheapest`, `available`), `max_tier` (1 strongest),
`min_context`, `max_ttft_ms`, `min_tps`, `no_training`, `free_only`, `max_cost` (USD a
request), `pinned` (tried first), `only_pinned`, `fallback` (tried last),
`hedge_after_ms` (ask the next model too when the first is slow to start).

The help of each page: `/help/start.md`, `/help/overview.md`, `/help/providers.md`,
`/help/filter.md`, `/help/pipe.md`; the list at `/help`.
