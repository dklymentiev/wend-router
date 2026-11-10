# Changelog

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and versions follow [Semantic Versioning](https://semver.org/).

## [0.2.0] - 2026-11-09

The first public release.

### Added

- One local endpoint for many LLM providers: OpenAI Chat Completions
  (`/v1/chat/completions`, `/v1/models`) and the Anthropic Messages API
  (`/v1/messages`, streaming and tool use).
- Fifteen providers known out of the box, three of them usable without a key;
  any OpenAI-compatible server can be added as your own.
- Automatic failover: a model that errors, is out of quota or stays silent is
  replaced by the next one before the caller sees anything; a model that
  stalls midway is cut off.
- Quota awareness from `Retry-After` and rate-limit headers, and learned
  pauses when a provider does not send them.
- Background checks of idle models and a test battery (tool calls, vision,
  JSON, instructions, reasoning, code) that records what each model can do.
  Both can be started and stopped from the dashboard.
- Profiles by capability, speed, context, price and training policy, with
  pinned and last-resort models; six built in.
- Hedged requests: a profile can ask a second model when the first is slow to
  start.
- Paid models only within a per-request cost cap and a daily budget.
- Filter: listed terms are replaced with placeholders before a request leaves
  and restored in the answer, streamed answers included; a test panel shows
  the round trip on a chosen model.
- Filter kinds: any email, card number, IBAN, SSN, phone, API key, password,
  IP, URL, and account, passport or birth-date numbers after their keyword,
  from an editable catalogue (`entities.json`) to which you can add your own.
- Dashboard: Simple (`/dashboard`) and Pro (`/pro`), with the Pipe, a live
  view of every request; the latest traffic is kept in memory.
- Usage per day, model and client; optional traffic recording to disk.
- A request is served only by what it asked for: a profile by its models,
  `provider/model` by exactly that model, a bare model name by that model at
  any provider; otherwise a quick error, and 404 for an unknown name.
- Help on every dashboard page, with a copy for an agent; the same guides as
  Markdown at `/help`.
- Installers for Linux, macOS (`install.sh`) and Windows (`install.ps1`): build
  from source with Go or in Docker, or fetch Go just for the build, or take
  the ready binary checked against `SHA256SUMS`.
- Release binaries for Linux, macOS and Windows on amd64 and arm64; examples
  for curl, Python, Node and coding agents.

[0.2.0]: https://github.com/dklymentiev/wend-router/releases/tag/v0.2.0
