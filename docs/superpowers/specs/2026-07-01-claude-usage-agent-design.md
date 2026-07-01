# Claude Usage Agent — 设计文档

- 日期：2026-07-01
- 状态：待实现
- 参考项目：`~/Projects/Claude-Code-Usage-Tracker`（Python，含菜单栏 GUI）

## 1. 目标

做一个 macOS 原生二进制 `claude-usage-agent`，定时采集 Claude Code（会员订阅）的用量数据，推送给一个本地 HTTP 服务。可配置成 macOS launchd 服务常驻运行。**不含菜单栏或任何 GUI。**

## 2. 背景与参考

参考项目的 `claude_usage.py` 做了两类事：

1. **采集**：从 macOS Keychain 读 Claude Code 的 OAuth token，调 `https://api.anthropic.com/api/oauth/usage`，拿 7 天配额 / 5 小时会话 / Extra Usage。
2. **本地统计**：扫描 `~/.claude/projects/*/*.jsonl`，按模型+日期汇总 token，算费用，写本地 JSON，做 daily/weekly 历史。

本项目**只保留采集与推送**，把所有历史/统计/费用逻辑移交给接收推送的服务端。这样二进制成为一个职责单一、无状态、易维护的「采集转发器」。

## 3. 总体架构

### 定位
无状态采集转发器：定时 `读 Keychain → 调 Usage API → POST 给 HTTP 服务`，附带 haiku 保活。

### 运行形态（子命令）

| 子命令     | 作用 |
|------------|------|
| `daemon`   | 常驻运行：内置 5 分钟采集 ticker + 独立节流的 haiku 保活 ticker；可被 launchd KeepAlive 拉起 |
| `collect`  | 单次采集并推送一次（调试/手动用），不进循环 |
| `install`  | 生成 `~/Library/LaunchAgents/com.user.claude-usage-agent.plist`（含 KeepAlive、RunAtLoad，把 flag 写进 ProgramArguments）并 `launchctl load` |
| `uninstall`| `launchctl unload` + 删 plist |
| `status`   | 查 launchd 里该 agent 的运行状态（pid / last exit code / state） |

### 外部依赖

- `security` 命令（读 Keychain）—— 与参考项目一致，不直接调 macOS Security.framework
- `claude` CLI —— 仅保活和 401 token 刷新时用
- 目标 HTTP 服务（默认 `http://localhost:8000/api/push/claude`）

### 产出
`go build` 直接产出 darwin/arm64 单文件二进制，无运行时依赖。

## 4. 数据流与 payload

### 采集流程（daemon 每 5 分钟一次；collect 单次）

1. 调 `security find-generic-password -s "Claude Code-credentials" -w` 读 Keychain → 解析 JSON → 取 `claudeAiOauth.accessToken`
2. `GET https://api.anthropic.com/api/oauth/usage`，header：
   - `Authorization: Bearer <token>`
   - `anthropic-beta: oauth-2025-04-20`
3. 401 时刷新 token：跑一次 `claude --print --model haiku -p "hi"`，重读 Keychain，重试一次 API
4. 成功后构造 payload 推送
5. 推送失败重试 3 次（指数退避 1s/2s/4s），仍失败记日志后跳过，等下一轮

### 推送 payload

```
POST <push-url>   （默认 http://localhost:8000/api/push/claude）
Content-Type: application/json

{
  "seven_day": {"utilization": 45.2, "resets_at": "2026-07-08T12:00:00Z"},
  "five_hour": {"utilization": 12.8, "resets_at": "2026-07-01T15:00:00Z"}
}
```

- **只推 sample 里的字段**：`seven_day.utilization`、`seven_day.resets_at`、`five_hour.utilization`、`five_hour.resets_at`
- `extra_usage` 不推（sample 未列）
- `resets_at` 原样透传（API 返回的 ISO 字符串），不做时区转换，交给服务端处理

### 本地存储

**不存任何本地文件**。所有历史、deltas、weekly 逻辑由接收推送的服务端负责。

## 5. 保活逻辑

- **采集 ticker**：默认 5 分钟
- **保活 ticker**：默认 30 分钟（独立节流，避免一天 288 次 haiku 调用）
- 保活 = `claude --print --model haiku -p "hi"`，capture output，30s 超时，失败静默记日志
- 两个 ticker 各跑一个 goroutine，通过共享 context 控制生命周期，互不阻塞

## 6. 配置（flag / 环境变量，无配置文件）

`daemon` 和 `collect` 的参数：

| flag                    | 环境变量                            | 默认                                          | 说明        |
|-------------------------|-------------------------------------|-----------------------------------------------|-------------|
| `--push-url`            | `CLAUDE_USAGE_PUSH_URL`             | `http://localhost:8000/api/push/claude`       | 推送目标    |
| `--interval`            | `CLAUDE_USAGE_INTERVAL`             | `5m`                                          | 采集间隔    |
| `--keepalive-interval`  | `CLAUDE_USAGE_KEEPALIVE_INTERVAL`   | `30m`                                         | 保活间隔    |
| `--keepalive`           | `CLAUDE_USAGE_KEEPALIVE`            | `true`                                        | 是否启用保活 |

优先级：**flag > 环境变量 > 默认**。`install` 子命令把当前生效的 flag 写进生成的 plist 的 `ProgramArguments`。

## 7. launchd 集成

### install
- plist 路径：`~/Library/LaunchAgents/com.user.claude-usage-agent.plist`
- Label：`com.user.claude-usage-agent`
- `ProgramArguments`：当前二进制绝对路径（`os.Executable()`）+ `daemon` + 用户传入且生效的 flag
- `KeepAlive=true`（崩溃自动拉起）、`RunAtLoad=true`
- `StandardOutPath` / `StandardErrorPath` 指向 `~/Library/Logs/claude-usage-agent.log`
- 写完执行 `launchctl load`
- 若已存在同名 agent，先 unload 再覆盖

### uninstall
`launchctl unload` + 删除 plist 文件。

### status
执行 `launchctl print gui/$(id -u)/com.user.claude-usage-agent`，解析并展示 pid / last exit code / state。agent 未加载时给出明确提示。

### 日志
daemon 用 `log` 标准库写 stdout/stderr，launchd 重定向到日志文件。每轮采集一行结构化日志：时间、ops、耗时、错误（如有）。

## 8. 代码结构

单 module，按职责分文件：

```
claude-sub-agent/
├── go.mod
├── main.go         # CLI 入口、子命令路由（flag 解析 → dispatch）
├── collector.go    # 采集核心：读 token、调 API、401 重试、构造 payload
├── pusher.go       # 推送 HTTP，重试退避
├── keepalive.go    # haiku 保活
├── daemon.go       # daemon 循环：两个 ticker + context 生命周期
├── launchd.go      # install/uninstall/status：生成 plist、launchctl 调用
├── *_test.go       # 对应测试
├── README.md
└── Makefile        # build / install / test
```

### 测试策略
- **collector**：`httptest.Server` mock Usage API，覆盖 成功 / 401 刷新成功 / 401 刷新失败 / 网络错
- **pusher**：mock 推送服务端，覆盖 成功 / 5xx 重试 / 超时
- **launchd**：plist 生成用字符串断言（不真跑 launchctl），写到 tempdir 验证内容
- 真实的 `security` / `claude` 命令通过接口抽象，测试注入 stub

## 9. 不做的事（YAGNI 边界）

- ❌ 菜单栏 / 任何 GUI
- ❌ 本地 JSON 存储（snapshots / daily / local / weekly 全不做）
- ❌ daily deltas / weekly 聚合 / 费用计算 / JSONL 扫描 / 定价表
- ❌ `report` 子命令（无本地数据可报）
- ❌ 多设备 / 多账号
- ❌ 通知 / 提醒
- ❌ Windows / Linux 支持（纯 macOS）

## 10. 验收标准

1. `go build` 产出可在 darwin/arm64 运行的单一二进制
2. `claude-usage-agent collect` 单次采集后，目标服务收到符合 sample 结构的 POST
3. `claude-usage-agent daemon` 在前台运行，每 5 分钟采集推送一次，每 30 分钟保活一次，日志可见
4. `claude-usage-agent install` 后 `launchctl list | grep claude-usage-agent` 能看到，重启后自动拉起
5. `uninstall` 后 agent 彻底移除
6. `status` 正确展示运行状态
7. 所有 `*_test.go` 通过 `go test ./...`
