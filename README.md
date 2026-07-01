# claude-usage-agent

A small, stateless macOS binary that periodically collects [Claude Code](https://www.anthropic.com/claude-code) subscription usage and POSTs it to a local HTTP service. Designed to run as a `launchd` agent. **No GUI, no local storage** — all history, deltas, and aggregation belong to the service that receives the pushes.

This is a Go reimplementation of the collection half of a reference Python tracker, stripped of the menu-bar app and local stats so the binary is a single-responsibility **collect-and-forward** agent.

## How it works

Every collection cycle:

1. Read the OAuth token from macOS Keychain (`security find-generic-password -s "Claude Code-credentials"`).
2. `GET https://api.anthropic.com/api/oauth/usage` with the bearer token.
3. On `401`, refresh the token by running a no-op `claude` CLI command, then retry the API once.
4. POST the payload to the configured push URL.

### Payload

```http
POST <push-url>   (default http://localhost:8000/api/push/claude)
Content-Type: application/json

{
  "seven_day": { "utilization": 45.2, "resets_at": "2026-07-08T12:00:00Z" },
  "five_hour": { "utilization": 12.8, "resets_at": "2026-07-01T15:00:00Z" }
}
```

Only `seven_day` and `five_hour` `utilization` + `resets_at` are forwarded. `resets_at` is passed through verbatim (no timezone conversion) — the receiving service owns all formatting.

## Install

```bash
make build          # produces ./claude-usage-agent (darwin/arm64)
make install        # builds, installs the binary, registers the launchd agent
```

`install` writes `~/Library/LaunchAgents/com.user.claude-usage-agent.plist` (with `KeepAlive` + `RunAtLoad`) and runs `launchctl load`. The flags you pass at install time are baked into the plist's `ProgramArguments`.

## Usage

```
claude-usage-agent <command> [flags]

Commands:
  daemon      Run continuously: collect every 5m, keepalive every 30m.
  collect     Collect once and push (debug / manual).
  install     Write the launchd plist (with current flags) and load it.
  uninstall   Unload the agent and delete the plist.
  status      Show the launchd status of the agent.
```

### Flags (`daemon` / `collect`)

| Flag                    | Env var                            | Default                                   | Description         |
| ----------------------- | ---------------------------------- | ----------------------------------------- | ------------------- |
| `--push-url`            | `CLAUDE_USAGE_PUSH_URL`            | `http://localhost:8000/api/push/claude`   | Push target         |
| `--interval`            | `CLAUDE_USAGE_INTERVAL`            | `5m`                                      | Collect interval    |
| `--keepalive-interval`  | `CLAUDE_USAGE_KEEPALIVE_INTERVAL`  | `30m`                                     | Keepalive interval  |
| `--keepalive`           | `CLAUDE_USAGE_KEEPALIVE`           | `true`                                    | Enable keepalive    |

**Precedence:** flag > env > default.

### Keepalive

To keep the 5-hour session window warm, the daemon runs `claude --print --model haiku -p "hi"` on its own 30-minute ticker, independent of the collect loop. This avoids hammering the CLI 288×/day while still keeping the token/session fresh.

### Logs

When run under launchd, stdout/stderr are redirected to `~/Library/Logs/claude-usage-agent.log`. Each cycle logs one structured line: timestamp, ops performed (`api-ok` / `refresh-token` / `api-failed`), elapsed time, and any error.

## Development

```bash
make test       # go test ./...
make vet        # go vet
make fmt        # gofmt -w
```

### Architecture

```
main.go       CLI entry, subcommand routing, flag+env config resolution
collector.go  Read token → call Usage API → refresh on 401 → build payload
pusher.go     HTTP POST with 3× exponential-backoff retry (1s/2s/4s)
keepalive.go  Haiku keepalive (best-effort, never fatal)
daemon.go     Two ticker goroutines sharing one cancellable context
launchd.go    install/uninstall/status: plist generation + launchctl
commands.go   Production wrappers around the `security` and `claude` commands
```

External commands (`security`, `claude`) and the Usage API are behind small interfaces (`TokenProvider`, `TokenRefresher`, `UsageAPI`), so the tests inject stubs and `httptest.Server` mocks instead of touching Keychain or the real network.

## Scope (what this is *not*)

- No menu bar or any GUI.
- No local JSON storage, daily deltas, weekly aggregation, cost calculation, or JSONL scanning — that's the receiving service's job.
- macOS only.

## Design

See [`docs/superpowers/specs/2026-07-01-claude-usage-agent-design.md`](docs/superpowers/specs/2026-07-01-claude-usage-agent-design.md) for the full design document.
