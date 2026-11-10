# Contributing

Thanks for looking. Issues and pull requests are welcome.

## Setup

Go 1.27 or newer. No other dependencies.

```
go build -o wend ./cmd/wend
go test ./...
```

The dashboard tests drive headless Chrome against a fake provider; they need
Node 20 or newer and Chrome:

```
node --experimental-websocket tests/ui/pro.mjs ./wend
node --experimental-websocket tests/ui/simple.mjs ./wend
```

They start their own router in a temporary folder and never touch real
providers or your keys.

## Rules of the code

- One package, standard library only. A new dependency needs a reason that a
  few lines of code cannot meet.
- Tests use the fake providers in `router_test.go` and `tests/ui/harness.mjs`;
  no test calls a real provider.
- The dashboard pages are plain HTML embedded in the binary and use the
  classes and tokens in `ui/kit.css`; a page adds only layout of its own.
- Settings are environment variables named `ROUTER_*`; a new one goes into
  `wend help` (the `usage` text in `main.go`) and the README.
- No real names, hosts or keys in code, tests or examples: use `example.com`,
  `Acme`, `192.0.2.0/24`.

## Sending a change

1. Open an issue first for anything larger than a fix, so we agree on the shape.
2. Keep a pull request to one change, with a test that fails without it.
3. Add a line to `CHANGELOG.md` under `[Unreleased]`.
4. `go vet ./...` and `go test ./...` pass.
