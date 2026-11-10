# Pipe: every request as it passes

The Pipe shows traffic live: what your tools asked, which models the router tried,
and the answer as it streams. The router's own background checks and tests show too,
marked **check** and **test**.

## Two views

- **Requests**: one card per request. At the top what was asked; below it each model
  tried, in order, with its status, how long it took and its answer as it was written.
  When the filter is on, **model got** shows the text as the model saw it, with
  placeholders. **Raw** opens every line exactly as it passed between the tool, the
  router and the model.
- **Outgoing**: one line per request sent to a model, newest on top, the text exactly
  as it left the router. Use it to check what really leaves your machine.

**Pause** stops the view from moving while you read; nothing is lost. **Clear**
empties the view.

## What is kept

The latest traffic (8 MB) is kept in memory, so it shows when you open the tab, also
for requests made before you opened it. A restart forgets it.

**Record traffic to disk** writes every request and response, as they were, to
`logs/traffic-YYYY-MM-DD.jsonl` next to the program, one file a day, kept 7 days.
It is off by default because prompts and answers then sit on disk. Keys are never
written.

## Who can see it

The Pipe shows what your tools send, so it opens on this machine only. To watch it
from another machine start the router with `ROUTER_REMOTE_PIPE=1`, and only behind an
access control of your own.

## Reading a failure

A model that failed shows its status and error, then the next model tried. Common
ones:

| You see | Means |
|---|---|
| 429, rate limit, quota | the provider's limit is used up; the model pauses until it resets |
| no first token within ... | it did not start answering in time and was replaced |
| sent nothing for ... mid-answer | it went silent and was cut off |
| 401, 403 | the key is wrong or lacks access |
| client-left | your tool stopped waiting before the answer was done |

## For an agent

```
# the live stream, as server-sent events (this machine only)
curl -sN http://127.0.0.1:8888/pipe

# record traffic to disk: on, off, and where it goes
curl -s -X POST http://127.0.0.1:8888/api/traffic -H "X-Router-UI: 1" -d '{"on": true}'
curl -s -X POST http://127.0.0.1:8888/api/traffic -H "X-Router-UI: 1" -d '{"on": false}'
curl -s http://127.0.0.1:8888/api/traffic
```

Changes need the header `X-Router-UI: 1` and are accepted from this machine only.
