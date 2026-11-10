# Notes for coding agents

wend is a single Go package (`package main`) in `cmd/wend/`, standard library only.
The files below live there; `tests/ui/` and the docs are at the top.

## Layout

| Files | What |
|---|---|
| `main.go` | entry point, commands, `wend help` text |
| `server.go`, `upstream.go`, `anthropic.go` | the API: request parsing, the try-next-model loop, streaming, the Anthropic Messages translation |
| `router.go`, `model.go`, `hedge.go`, `limits.go`, `classify.go` | choosing a model, model state, hedged requests, quotas, error classes |
| `probe.go`, `testmode.go`, `testrun.go`, `domains.go` | background checks, the test battery, runs started from the dashboard, what a model is good for |
| `profiles.go`, `profiles_api.go` | profiles and their API |
| `provider.go`, `providers.go`, `providers_api.go`, `seed.go`, `seed.json`, `keystore.go`, `seal_*.go`, `envfile.go` | providers, keys and their storage |
| `filter.go`, `entities.go`, `entities.json`, `pipeline.go` | the term filter and its catalogue of kinds of values, as a step every request and answer passes |
| `pipe.go`, `traffic.go`, `live.go`, `usage.go`, `cost.go`, `account.go`, `sessions.go`, `state.go`, `reputation.go` | observing: the Pipe, traffic log, live status, usage, spend (ours and what a provider says the key spent), sessions, saved state |
| `dashboard.html` (Pro), `simple.html` (Simple), `ui/kit.css`, `ui.go` | the dashboard, embedded in the binary |
| `*_test.go`, `tests/ui/` | Go tests with fake providers; dashboard tests driving headless Chrome |

## Rules

- Run `go vet ./...` and `go test ./...` before you finish; for dashboard
  changes also the two `tests/ui/*.mjs` scripts (see CONTRIBUTING.md).
- A bug fix comes with a test that fails without it.
- Tests never call real providers.
- New settings: `ROUTER_*`, listed in `wend help` and the README.
- Dashboard pages use the classes and tokens in `ui/kit.css`; no colours or
  sizes of their own.
- No real names, hosts, IPs or keys in code, tests or examples.
