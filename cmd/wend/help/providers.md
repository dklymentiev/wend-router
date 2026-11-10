# Providers: where the models come from

A provider is a service that runs models: Groq, Google AI Studio, OpenRouter and so on.
Wend router reads each provider's model list and serves those models through one
address. More providers mean more models and more room before limits run out.

## Start without keys

Kilo and OVHcloud work without a key, so the router answers from the first minute.
Ollama on this machine is found by itself when it runs.

## Add a key

1. Click a provider, then its sign-up link. Most give a free key in a minute.
2. Paste the key under **Add key** (a label is optional, say "personal"), press
   **Add key**.
3. Its models appear within seconds and are checked in the background.

A provider can hold several keys; the router uses them in turn. Keys are stored
encrypted in `keys.dat` on this machine (Windows protects it with your login; elsewhere
a key file in your user folder) and are never shown in full again. Remove one with
its **Remove** button. A key that comes from `wend.env` has none: delete its line in
that file.

Keys can also go in `wend.env` next to the program, one per line:

```
GROQ_API_KEY=...
GEMINI_API_KEY=...
OPENROUTER_API_KEY=...
```

| Provider | Variable |
|---|---|
| Groq | `GROQ_API_KEY` |
| Google AI Studio | `GEMINI_API_KEY` |
| OpenRouter | `OPENROUTER_API_KEY` |
| NVIDIA | `NVIDIA_API_KEY` |
| Mistral | `MISTRAL_API_KEY` |
| SambaNova | `SAMBANOVA_API_KEY` |
| Cloudflare Workers AI | `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID` |
| Z.ai | `ZAI_API_KEY` |
| Requesty | `REQUESTY_API_KEY` |
| Ollama Cloud | `OLLAMA_API_KEY` |
| LLM7 | `LLM7_API_KEY` |
| Hetzner | `HETZNER_INFERENCE_TOKEN` |
| Kilo (optional) | `KILO_API_KEY` |

Cloudflare's address needs your account id: fill it in the field under the address.

## "May train on prompts"

Some free tiers may use what you send to train their models. Those providers are
marked. For anything you would not post publicly, use the `private` profile (only
providers that do not train) or start the router with `ROUTER_ALLOW_TRAINING=0`.

## Turn a provider off

The switch next to a provider stops its use without removing its keys. Turn it back on
any time.

## Your own server

**+ Add provider** takes any OpenAI-compatible API that lists its models at
`GET /models`: a local Ollama or LM Studio, a company gateway, a paid service.

- **Id**: a short name (`lmstudio`), used in model names as `lmstudio/model`.
- **Address**: the API base, ending in `/v1`; `https://`, or `http://localhost` for a
  server on this machine.
- **Key**: if the server needs one.
- **Models to use**: all it lists, or only those priced zero.

## Paid models

Paid models are never used unless you allow a price: `ROUTER_MAX_COST` (USD per
request) or a profile's **Price** limit. Then they come after free ones, within
`ROUTER_DAILY_BUDGET` (USD a day, 1 by default).

## For an agent

Changes need the header `X-Router-UI: 1` and are accepted from this machine only.

```
# providers, their state and model counts (keys never in full)
curl -s http://127.0.0.1:8888/api/providers

# add a key to a provider
curl -s -X POST http://127.0.0.1:8888/api/providers/groq/keys -H "X-Router-UI: 1" \
  -d '{"key": "<the key>", "label": "personal"}'

# a value the provider's address needs (Cloudflare's account id)
curl -s -X POST http://127.0.0.1:8888/api/providers/cloudflare/vars -H "X-Router-UI: 1" \
  -d '{"CLOUDFLARE_ACCOUNT_ID": "<account id>"}'

# turn a provider off and on
curl -s -X POST http://127.0.0.1:8888/api/providers/kilo/enabled -H "X-Router-UI: 1" -d '{"enabled": false}'
curl -s -X POST http://127.0.0.1:8888/api/providers/kilo/enabled -H "X-Router-UI: 1" -d '{"enabled": true}'

# add your own OpenAI-compatible server
curl -s -X POST http://127.0.0.1:8888/api/providers -H "X-Router-UI: 1" \
  -d '{"id": "lmstudio", "base_url": "http://localhost:1234/v1", "discover": "all"}'
```

Never print a key back to the user once it is stored.
