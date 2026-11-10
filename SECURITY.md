# Security

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting on this repository
(Security tab, "Report a vulnerability"). Do not open a public issue for it.
You will get an answer within a week.

Only the latest release is supported.

## What wend protects, and what it does not

- **Keys.** Keys added in the dashboard are stored encrypted in `keys.dat`:
  with DPAPI on Windows (only the same user on the same machine can open it),
  with AES-GCM elsewhere, the key kept in a separate file
  (`ROUTER_MASTER_KEY_FILE`). Anyone who can read both files, or run code as
  your user, can read the keys. Keys in `wend.env` are plain text.
- **Network.** By default wend listens on 127.0.0.1. When you open it to a
  network, set `ROUTER_TOKEN`; without it anyone who reaches the port can use
  your providers. Traffic between your agent and wend is plain HTTP; put TLS
  in front if it crosses a network you do not trust.
- **Dashboard.** Changes (keys, providers, profiles, filter) are accepted from
  this machine only unless `ROUTER_REMOTE_UI=1`; the Pipe, which shows prompts
  and answers, likewise unless `ROUTER_REMOTE_PIPE=1`. The dashboard has no
  login of its own: turn those on only behind an access control.
- **Providers.** Prompts go to third parties. Some free tiers may train on
  them; they are marked, and `private` or `ROUTER_ALLOW_TRAINING=0` avoids
  them.
- **Filter.** It replaces the terms you list, and only those; it is a help,
  not a guarantee that nothing personal leaves.
- **Traffic recording** is off by default; when on, prompts and answers are
  written to disk unencrypted.
