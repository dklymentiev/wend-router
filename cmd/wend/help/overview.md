# Overview: how Wend router works

## What happens to a request

1. Your tool sends a request with a **model** name.
2. The name says what may answer: a **profile** (only its models), a
   **provider/model** (exactly that one), or a **model name** (that model at any
   provider). Nothing outside it is ever used.
3. Among those, the router takes the models that are up right now, in the profile's
   order: pinned models first, then those whose last attempt worked, then untried
   ones, then ones that failed last.
4. It sends the request to the first. Until the first word of the answer arrives, a
   model that errors, is out of quota or stays silent is replaced by the next one;
   your tool does not notice. A model that goes quiet in the middle of an answer is
   cut off after 45 seconds.
5. If none can answer, your tool gets an error within seconds.

In the background the router checks idle models with a short request and runs a
small test (tool calls, images, JSON, instructions, reasoning) on each, so it knows
what works before your tool needs it.

## The cards at the top

| Card | Shows |
|---|---|
| Requests in flight | requests being answered right now |
| Requests per minute | model attempts in the last minute |
| Tokens per minute | tokens in and out in the last minute |
| Answered | share of requests that got an answer, last 5 minutes |
| First token | typical wait for the first word, last 5 minutes |
| Spent today | money spent on paid models, against the daily budget |
| Active model | the model `auto` would pick now |

## Requests and Live requests

The chart counts requests over time: served, failed, and the router's own checks and
tests. **Live requests** lists the latest, newest first: which model served each,
how long the first word took, the whole answer, speed and cost.

## Clients and sessions

Every tool that used the router in the last 15 minutes. A session is one conversation;
it stays on one model while that model answers, so the conversation does not jump
between models. A tool can name itself with the header `X-Router-Client` and its
conversation with `X-Session-Id`.

## Profiles

What your tools ask for. Click one to change it: what its models must be able to do,
how to order them, which providers, limits on speed and price, pinned models (star,
tried first) and reserves (arrow, tried last). The panel shows which models the
profile takes right now as you change it.

## Models

Every model the router knows, one row each. Click a header to sort; the table scrolls
inside its frame.

| Column | Means |
|---|---|
| State | **Up**; **Quota** (paused until its limit resets); **Cooling down** (failed several times, rests a while); **Last check failed**; **Unavailable** (the provider no longer serves it); **Provider off**; **Untested** |
| Tier | strength, 1 strongest: measured by the tests, else from the built-in list |
| Score | share of the tests passed; the arrow retests it |
| First token, Tok/s | how fast it starts, and how fast it writes |
| Cost / request | for 1K tokens in and 1K out; free models say free |
| Good for | what it is suited to, from its name, abilities and speed |
| Avail, Usually, OK / Err | how it did recently, and over days |
| Quota left | what the provider says is left in its limit |

**Only models that have served or failed** hides the ones nothing has used yet.
**Quick check** sends one short request to each model; **Full test** runs the test
battery (8 requests a model). Both can be stopped.

Tick models in the first column and press **New profile from selected** to make a
profile of exactly those.

## Speed: which "fast" you need

`fastest` orders models by how quickly they start answering. For long answers what
matters more is **Tok/s**: a model that writes 25 tokens a second needs over a minute
for 2,000 tokens. For such work give the profile a **Speed** limit (`min_tps`), so slow
writers are left out.

## For an agent

```
# everything this page shows, as JSON
curl -s http://127.0.0.1:8888/status.json

# usage by day
curl -s http://127.0.0.1:8888/api/usage

# a quick check or a full test of one model (free models), and stopping a test
curl -s -X POST "http://127.0.0.1:8888/api/probe?model=groq/openai/gpt-oss-20b" -H "X-Router-UI: 1"
curl -s -X POST "http://127.0.0.1:8888/api/test?model=groq/openai/gpt-oss-20b" -H "X-Router-UI: 1"
curl -s -X POST http://127.0.0.1:8888/api/test/stop -H "X-Router-UI: 1"

# a profile of exactly two models, the first tried first
curl -s -X PUT http://127.0.0.1:8888/api/profiles/pair -H "X-Router-UI: 1" \
  -d '{"pinned": ["groq/openai/gpt-oss-120b", "groq/openai/gpt-oss-20b"], "only_pinned": true}'
```

Changes need the header `X-Router-UI: 1` and are accepted from this machine only.
Profile fields are listed in `/help/start.md`.
