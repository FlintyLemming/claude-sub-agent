# claude-usage-agent

A small, stateless binary (macOS + Windows) that periodically collects [Claude Code](https://www.anthropic.com/claude-code) subscription usage and POSTs it to a local HTTP service. Runs as a `launchd` agent on macOS or a Task Scheduler logon task on Windows. **No GUI, no local storage** — all history, deltas, and aggregation belong to the service that receives the pushes.

This is a Go reimplementation of the collection half of a reference Python tracker, stripped of the menu-bar app and local stats so the binary is a single-responsibility **collect-and-forward** agent.

## How it works

Every collection cycle:

1. Read the OAuth token + `expiresAt` from the Claude Code credentials source:
   - macOS: both the Keychain (`security find-generic-password -s "Claude Code-credentials"`, where the CLI keeps its live token) and `$CLAUDE_CONFIG_DIR/.credentials.json` (default `~/.claude/`); whichever token expires later wins, so a stale leftover file can't shadow the live Keychain token.
   - Windows: the credentials file only (there is no Keychain equivalent).
2. If `expiresAt` is within 5 minutes (the CLI's own refresh window), have the CLI refresh it first — see [Token refresh](#token-refresh). The stale token is never sent (expired tokens have been reported to get a `429` from the usage endpoint rather than a `401`, which would look like rate limiting).
3. `GET https://api.anthropic.com/api/oauth/usage` with the bearer token and a `claude-code/<version>` User-Agent.
4. On `401`, refresh the same way and retry the API once.
5. If a refresh fails, leaves the token expired, or the API still says `401`, back off: cycles that see the same token skip the CLI and the API for 5m, 10m, 20m, 40m, then hourly. A new token appearing (the user ran the CLI) lifts the backoff at once. Hammering a known-bad token is what provokes upstream 429s.
6. On `429`, back off honoring the server's `Retry-After` header (clamped between the collect interval and 30m); without the header, the wait doubles per consecutive 429.
7. POST the payload to the configured push URL.

### Token refresh

The access token lives about 8 hours, and only a refresh with the stored refresh token extends it — so no keepalive is needed, just a refresh when the token is due. The agent never calls the token endpoint itself: the refresh token is single-use, and rotating it behind the CLI's back makes the CLI's next refresh fail with `invalid_grant` and forces a `/login`. Instead it runs:

```
claude -p /usage --model claude-usage-agent-no-inference --output-format json \
       --no-session-persistence --strict-mcp-config
```

`/usage` is a local command in print mode: it makes **no model call** (`num_turns: 0`, `total_cost_usd: 0`), but its usage fetch refreshes an expired token through the CLI's own path — the shared `~/.claude/.oauth_refresh.lock`, a re-read under that lock, and a write-back to the store the CLI reads (Keychain on macOS). The agent checks the JSON's `local_command` field; should some CLI version forward the text to the model instead, the nonexistent `--model` makes that request fail with a 404 before any quota is spent. `claude update`, used by earlier versions, never touches OAuth, which is why collection used to stop about 8 hours after the CLI was last used.

There is **no keepalive** and no model invocation anywhere in the agent: a periodic prompt would burn subscription quota, start 5-hour windows the user never opened (distorting the very numbers being collected), and invite rate limiting.

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
commands.go               Cross-platform wrappers: credentials file, `claude` CLI refresh, User-Agent
tokens_darwin.go          macOS credential sources (Keychain + file, later expiry wins)
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
