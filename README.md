# claude-usage-agent

A small, stateless binary (macOS + Windows) that periodically collects [Claude Code](https://www.anthropic.com/claude-code) subscription usage and POSTs it to a local HTTP service. Runs as a `launchd` agent on macOS or a Task Scheduler logon task on Windows. **No GUI, no local storage** — all history, deltas, and aggregation belong to the service that receives the pushes.

This is a Go reimplementation of the collection half of a reference Python tracker, stripped of the menu-bar app and local stats so the binary is a single-responsibility **collect-and-forward** agent.

## How it works

Every collection cycle:

1. Read the OAuth token + `expiresAt` from the Claude Code credentials source:
   - macOS: `$CLAUDE_CONFIG_DIR/.credentials.json` (default `~/.claude/`), falling back to Keychain (`security find-generic-password -s "Claude Code-credentials"`).
   - Windows: the credentials file only (there is no Keychain equivalent).
2. If `expiresAt` says the token is stale (60s leeway), run `claude update` first — the CLI rotates the token as a side effect, at zero model-quota cost. The stale token is never sent.
3. `GET https://api.anthropic.com/api/oauth/usage` with the bearer token and a `claude-code/<version>` User-Agent.
4. On `401`, run `claude update` and retry the API once. If it's *still* unauthorized, the agent goes quiet: no further API calls until the on-disk token actually changes (normal Claude Code use rotates it). Hammering a known-bad token is what provokes upstream 429s.
5. On `429`, back off honoring the server's `Retry-After` header (clamped between the collect interval and 30m); without the header, the wait doubles per consecutive 429.
6. POST the payload to the configured push URL.

There is **no keepalive** and no model invocation anywhere in the agent — surveying the ecosystem (Claude-Code-Usage-Monitor, Claude-Usage-Tracker, usage-monitor-for-claude) showed none of them call the model, and doing so both burns subscription quota and invites rate limiting.

### Payload

The push target is an [ai-plan-insight](../ai-plan-insight) **v2** push endpoint: `POST /api/push/v2/{instance_id}`, where `instance_id` is a `type: "claude", mode: "push"` instance registered in the server's `config.v2.json`.

```http
POST <push-url>   (default http://localhost:8000/api/push/v2/claude-personal)
Content-Type: application/json
Authorization: Bearer <push-token>

{
  "seven_day": { "utilization": 45.2, "resets_at": "2026-07-08T12:00:00Z" },
  "five_hour": { "utilization": 12.8, "resets_at": "2026-07-01T15:00:00Z" },
  "fable":     { "utilization": 44.0, "resets_at": "2026-07-26T07:00:00Z" }
}
```

The body matches the server's `ClaudePushRequest` schema: `seven_day` and `five_hour` are **both required**, so a usage response missing either window fails the cycle instead of pushing a payload the server would reject with `422`. `fable` (the model-scoped weekly window) is optional and omitted when the account has no such cap. `resets_at` is passed through verbatim (no timezone conversion) — the receiving service owns all formatting.

The Bearer token must match the server's `push_auth_secret`. When the server runs with `enforce_push_auth: false` the token may be omitted; with `enforce_push_auth: true` a missing/wrong token gets `401` (not retried).

## Install

macOS:

```bash
make build          # produces ./claude-usage-agent (darwin/arm64)
make install        # builds, installs the binary, registers the launchd agent
```

`install` writes `~/Library/LaunchAgents/com.user.claude-usage-agent.plist` (with `KeepAlive` + `RunAtLoad`) and runs `launchctl load`. The flags you pass at install time are baked into the plist's `ProgramArguments`.

Windows:

```bash
make build-windows  # cross-compiles claude-usage-agent.exe (amd64)
```

Copy the exe over, then on the Windows machine run `claude-usage-agent.exe install [flags]` — this registers (and immediately starts) a Task Scheduler logon task named `claude-usage-agent`. `uninstall` / `status` work the same way.

## Usage

```
claude-usage-agent <command> [flags]

Commands:
  daemon      Run continuously: collect every 5m.
  collect     Collect once and push (debug / manual).
  install     Register the background service (with current flags).
  uninstall   Unregister the background service.
  status      Show the service status.
```

### Flags (`daemon` / `collect`)

| Flag           | Env var                   | Default                                             | Description                                  |
| -------------- | ------------------------- | --------------------------------------------------- | -------------------------------------------- |
| `--push-url`   | `CLAUDE_USAGE_PUSH_URL`   | `http://localhost:8000/api/push/v2/claude-personal` | v2 push endpoint (`/api/push/v2/{instance}`) |
| `--push-token` | `CLAUDE_USAGE_PUSH_TOKEN` | (empty)                                             | Bearer token = server `push_auth_secret`     |
| `--interval`   | `CLAUDE_USAGE_INTERVAL`   | `5m`                                                | Collect interval                             |

**Precedence:** flag > env > default. The old `--keepalive` / `--keepalive-interval` flags are still accepted (so a service definition written by an older version keeps running) but are ignored.

### Logs

Under launchd, stdout/stderr are redirected to `~/Library/Logs/claude-usage-agent.log`. Each cycle logs one structured line: timestamp, ops performed (`api-ok` / `refresh-token` / `fable-ok` / `api-failed`), elapsed time, and any error.

## Development

```bash
make test       # go test ./...
make vet        # go vet
make fmt        # gofmt -w
```

### Architecture

```
main.go                   CLI entry, subcommand routing, flag+env config resolution
collector.go              Read token → expiry check → call Usage API → refresh on 401 → build payload
pusher.go                 HTTP POST with 3× exponential-backoff retry (1s/2s/4s)
daemon.go                 Collect loop with Retry-After-aware 429 backoff
commands.go               Cross-platform wrappers: credentials file, `claude` CLI, User-Agent
tokens_darwin.go          macOS credential chain (file → Keychain)
tokens_windows.go         Windows credential source (file only)
service.go                Shared service-manager plumbing (shellRunner, statusInfo)
service_darwin.go         launchd: plist generation + launchctl
service_windows.go        Task Scheduler entry points
service_windows_core.go   schtasks logic (tag-free so it's unit-tested on any OS)
```

External commands (`security`, `claude`, `launchctl`, `schtasks`) and the Usage API are behind small interfaces (`TokenProvider`, `TokenRefresher`, `UsageAPI`, `shellRunner`), so the tests inject stubs and `httptest.Server` mocks instead of touching the Keychain, Task Scheduler, or the real network.

## Scope (what this is *not*)

- No menu bar or any GUI.
- No local JSON storage, daily deltas, weekly aggregation, cost calculation, or JSONL scanning — that's the receiving service's job.

## Design

See [`docs/superpowers/specs/2026-07-01-claude-usage-agent-design.md`](docs/superpowers/specs/2026-07-01-claude-usage-agent-design.md) for the full design document (predates the keepalive removal and Windows port).
