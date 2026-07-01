# Claude Usage Agent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Create a stateless macOS native daemon (`claude-usage-agent`) written in Go to periodically collect Claude Code usage from Anthropic's OAuth API (refreshing token via `claude` CLI if needed), and POST the usage data to a local HTTP backend.

**Architecture:** A lightweight command-line utility with five subcommands (`daemon`, `collect`, `install`, `uninstall`, `status`). It uses interfaces to abstract external commands and HTTP requests for testability. Ticker loops run in concurrent goroutines with context cancellation and OS signal management.

**Tech Stack:** Go (Standard Library only), launchd for macOS service daemon.

---

### Task 1: Module Initialisation & Config / Types

**Files:**
- Create: `types.go`
- Create: `go.mod`
- Create: `types_test.go`

- [x] **Step 1: Write the failing test for OSCommandExecutor**

Create `types_test.go`:
```go
package main

import (
	"context"
	"strings"
	"testing"
)

func TestOSCommandExecutor(t *testing.T) {
	executor := &OSCommandExecutor{}
	out, err := executor.Run(context.Background(), "echo", "hello-world-test")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	got := strings.TrimSpace(string(out))
	if got != "hello-world-test" {
		t.Errorf("expected 'hello-world-test', got %q", got)
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test -v ./...`
Expected: FAIL with undefined: OSCommandExecutor

- [x] **Step 3: Write minimal implementation**

Create `go.mod`:
```go
module claude-usage-agent

go 1.20
```

Create `types.go`:
```go
package main

import (
	"context"
	"os/exec"
	"time"
)

type Config struct {
	PushURL           string
	Interval          time.Duration
	KeepAliveInterval time.Duration
	KeepAlive         bool
}

type UsagePeriod struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type UsageResponse struct {
	SevenDay UsagePeriod `json:"seven_day"`
	FiveHour UsagePeriod `json:"five_hour"`
}

type CommandExecutor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type OSCommandExecutor struct{}

func (e *OSCommandExecutor) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Output()
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test -v ./...`
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add go.mod types.go types_test.go
git commit -m "feat: initialize module and types with OSCommandExecutor"
```

---

### Task 2: Keychain Token Extraction

**Files:**
- Create: `token.go`
- Create: `token_test.go`

- [x] **Step 1: Write the failing test**

Create `token_test.go`:
```go
package main

import (
	"context"
	"errors"
	"testing"
)

type MockCommandExecutor struct {
	RunFunc func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (m *MockCommandExecutor) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return m.RunFunc(ctx, name, args...)
}

func TestReadToken_Success(t *testing.T) {
	mockExec := &MockCommandExecutor{
		RunFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "security" && len(args) >= 4 && args[0] == "find-generic-password" && args[2] == "Claude Code-credentials" {
				return []byte(`{"claudeAiOauth":{"accessToken":"oauth-test-token-val"}}`), nil
			}
			return nil, errors.New("unexpected command")
		},
	}
	token, err := ReadToken(context.Background(), mockExec)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if token != "oauth-test-token-val" {
		t.Errorf("expected 'oauth-test-token-val', got %q", token)
	}
}

func TestReadToken_CmdFailure(t *testing.T) {
	mockExec := &MockCommandExecutor{
		RunFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, errors.New("keychain entry not found")
		},
	}
	_, err := ReadToken(context.Background(), mockExec)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test -v ./...`
Expected: FAIL with undefined: ReadToken

- [x] **Step 3: Write minimal implementation**

Create `token.go`:
```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

type KeychainJSON struct {
	ClaudeAiOauth struct {
		AccessToken string `json:"accessToken"`
	} `json:"claudeAiOauth"`
}

func ReadToken(ctx context.Context, exec CommandExecutor) (string, error) {
	out, err := exec.Run(ctx, "security", "find-generic-password", "-s", "Claude Code-credentials", "-w")
	if err != nil {
		return "", err
	}
	clean := strings.TrimSpace(string(out))
	if clean == "" {
		return "", errors.New("empty credentials output from keychain")
	}
	var data KeychainJSON
	if err := json.Unmarshal([]byte(clean), &data); err != nil {
		return "", err
	}
	token := data.ClaudeAiOauth.AccessToken
	if token == "" {
		return "", errors.New("accessToken is empty in keychain credentials JSON")
	}
	return token, nil
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test -v ./...`
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add token.go token_test.go
git commit -m "feat: implement ReadToken helper to read oauth accessToken from macOS keychain"
```

---

### Task 3: Usage API Client with 401 Refresh

**Files:**
- Create: `collector.go`
- Create: `collector_test.go`

- [x] **Step 1: Write the failing test**

Create `collector_test.go`:
```go
package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCollectUsage_SuccessOnFirstTry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mock-token-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"seven_day": {"utilization": 45.2, "resets_at": "2026-07-08T12:00:00Z"},
			"five_hour": {"utilization": 12.8, "resets_at": "2026-07-01T15:00:00Z"}
		}`))
	}))
	defer server.Close()

	mockExec := &MockCommandExecutor{
		RunFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte(`{"claudeAiOauth":{"accessToken":"mock-token-1"}}`), nil
		},
	}

	collector := NewUsageCollector(mockExec, http.DefaultClient, server.URL)
	usage, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if usage.SevenDay.Utilization != 45.2 || usage.FiveHour.ResetsAt != "2026-07-01T15:00:00Z" {
		t.Errorf("unexpected usage data parsed: %+v", usage)
	}
}

func TestCollectUsage_RefreshOn401(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			if r.Header.Get("Authorization") != "Bearer mock-old-token" {
				t.Errorf("expected first attempt to use mock-old-token, got %q", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer mock-new-token" {
			t.Errorf("expected second attempt to use mock-new-token, got %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"seven_day": {"utilization": 10.0, "resets_at": "2026-07-08T12:00:00Z"},
			"five_hour": {"utilization": 5.0, "resets_at": "2026-07-01T15:00:00Z"}
		}`))
	}))
	defer server.Close()

	keychainCount := 0
	claudeCount := 0
	mockExec := &MockCommandExecutor{
		RunFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "security" {
				keychainCount++
				if keychainCount == 1 {
					return []byte(`{"claudeAiOauth":{"accessToken":"mock-old-token"}}`), nil
				}
				return []byte(`{"claudeAiOauth":{"accessToken":"mock-new-token"}}`), nil
			}
			if name == "claude" {
				claudeCount++
				return []byte("haiku keepalive output"), nil
			}
			return nil, errors.New("unexpected command")
		},
	}

	collector := NewUsageCollector(mockExec, http.DefaultClient, server.URL)
	usage, err := collector.Collect(context.Background())
	if err != nil {
		t.Fatalf("expected successful collection after refresh, got %v", err)
	}
	if usage.SevenDay.Utilization != 10.0 {
		t.Errorf("expected usage utilization 10.0, got %f", usage.SevenDay.Utilization)
	}
	if claudeCount != 1 {
		t.Errorf("expected claude refresh command to be run exactly 1 time, got %d", claudeCount)
	}
	if keychainCount != 2 {
		t.Errorf("expected keychain to be read 2 times, got %d", keychainCount)
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test -v ./...`
Expected: FAIL with undefined: NewUsageCollector

- [x] **Step 3: Write minimal implementation**

Create `collector.go`:
```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type UsageCollector struct {
	exec   CommandExecutor
	client *http.Client
	apiURL string
}

func NewUsageCollector(exec CommandExecutor, client *http.Client, apiURL string) *UsageCollector {
	if apiURL == "" {
		apiURL = "https://api.anthropic.com/api/oauth/usage"
	}
	if client == nil {
		client = &http.Client{}
	}
	return &UsageCollector{
		exec:   exec,
		client: client,
		apiURL: apiURL,
	}
}

func (c *UsageCollector) Collect(ctx context.Context) (*UsageResponse, error) {
	token, err := ReadToken(ctx, c.exec)
	if err != nil {
		return nil, fmt.Errorf("failed to read token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", c.apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		// Run refresh command: claude --print --model haiku -p "hi"
		_, refreshErr := c.exec.Run(ctx, "claude", "--print", "--model", "haiku", "-p", "hi")
		if refreshErr != nil {
			return nil, fmt.Errorf("failed to run refresh command: %w", refreshErr)
		}

		// Re-read token from Keychain
		token, err = ReadToken(ctx, c.exec)
		if err != nil {
			return nil, fmt.Errorf("failed to re-read token: %w", err)
		}

		// Re-request API
		req, err = http.NewRequestWithContext(ctx, "GET", c.apiURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")

		resp, err = c.client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status from Usage API: %d", resp.StatusCode)
	}

	var usage UsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&usage); err != nil {
		return nil, fmt.Errorf("failed to decode usage JSON: %w", err)
	}

	return &usage, nil
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test -v ./...`
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add collector.go collector_test.go
git commit -m "feat: implement CollectUsage client with oauth refresh on 401"
```

---

### Task 4: Pusher HTTP client with exponential backoff

**Files:**
- Create: `pusher.go`
- Create: `pusher_test.go`

- [x] **Step 1: Write the failing test**

Create `pusher_test.go`:
```go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPusher_Push_SuccessImmediately(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	pusher := NewPusher(nil)
	pusher.backoffs = []time.Duration{0}

	data := &UsageResponse{
		SevenDay: UsagePeriod{Utilization: 50.0, ResetsAt: "2026-07-08T12:00:00Z"},
		FiveHour: UsagePeriod{Utilization: 10.0, ResetsAt: "2026-07-01T15:00:00Z"},
	}

	err := pusher.Push(context.Background(), server.URL, data)
	if err != nil {
		t.Fatalf("expected success, got err: %v", err)
	}
}

func TestPusher_Push_RetryAndSucceed(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var received UsageResponse
		json.NewDecoder(r.Body).Decode(&received)
		if received.SevenDay.Utilization != 12.3 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	pusher := NewPusher(nil)
	pusher.backoffs = []time.Duration{0, 1 * time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}

	data := &UsageResponse{
		SevenDay: UsagePeriod{Utilization: 12.3, ResetsAt: "2026-07-08T12:00:00Z"},
	}

	err := pusher.Push(context.Background(), server.URL, data)
	if err != nil {
		t.Fatalf("expected success on third try, got err: %v", err)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

func TestPusher_Push_FailAll(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	pusher := NewPusher(nil)
	pusher.backoffs = []time.Duration{0, 1 * time.Millisecond, 2 * time.Millisecond}

	data := &UsageResponse{}
	err := pusher.Push(context.Background(), server.URL, data)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts total, got %d", attempts)
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test -v ./...`
Expected: FAIL with undefined: NewPusher

- [x] **Step 3: Write minimal implementation**

Create `pusher.go`:
```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Pusher struct {
	client   *http.Client
	backoffs []time.Duration
}

func NewPusher(client *http.Client) *Pusher {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Pusher{
		client:   client,
		backoffs: []time.Duration{0, 1 * time.Second, 2 * time.Second, 4 * time.Second},
	}
}

func (p *Pusher) Push(ctx context.Context, pushURL string, data *UsageResponse) error {
	body, err := json.Marshal(data)
	if err != nil {
		return err
	}

	var lastErr error
	for i, delay := range p.backoffs {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, "POST", pushURL, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := p.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("pusher: target server returned status %d", resp.StatusCode)
		if i == len(p.backoffs)-1 {
			break
		}
	}

	return fmt.Errorf("pusher retry exhaustion: %w", lastErr)
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test -v ./...`
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add pusher.go pusher_test.go
git commit -m "feat: implement Pusher HTTP client with exponential backoff retries"
```

---

### Task 5: Keepalive component

**Files:**
- Create: `keepalive.go`
- Create: `keepalive_test.go`

- [x] **Step 1: Write the failing test**

Create `keepalive_test.go`:
```go
package main

import (
	"context"
	"errors"
	"testing"
)

func TestKeepAlive_Success(t *testing.T) {
	mockExec := &MockCommandExecutor{
		RunFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "claude" && len(args) == 6 && args[0] == "--print" && args[1] == "--model" && args[2] == "haiku" && args[4] == "-p" {
				return []byte("haiku text reply"), nil
			}
			return nil, errors.New("unexpected arguments")
		},
	}
	manager := NewKeepAliveManager(mockExec)
	err := manager.KeepAlive(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestKeepAlive_Failure(t *testing.T) {
	mockExec := &MockCommandExecutor{
		RunFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, errors.New("claude executable not found")
		},
	}
	manager := NewKeepAliveManager(mockExec)
	err := manager.KeepAlive(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test -v ./...`
Expected: FAIL with undefined: NewKeepAliveManager

- [x] **Step 3: Write minimal implementation**

Create `keepalive.go`:
```go
package main

import (
	"context"
	"time"
)

type KeepAliveManager struct {
	exec CommandExecutor
}

func NewKeepAliveManager(exec CommandExecutor) *KeepAliveManager {
	return &KeepAliveManager{exec: exec}
}

func (k *KeepAliveManager) KeepAlive(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	_, err := k.exec.Run(ctx, "claude", "--print", "--model", "haiku", "-p", "hi")
	return err
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test -v ./...`
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add keepalive.go keepalive_test.go
git commit -m "feat: implement KeepAliveManager to ping Claude API with haiku command"
```

---

### Task 6: Daemon component (Tickers & Context lifecycle)

**Files:**
- Create: `daemon.go`
- Create: `daemon_test.go`

- [x] **Step 1: Write the failing test**

Create `daemon_test.go`:
```go
package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDaemon_StartAndCancel(t *testing.T) {
	var pushCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			atomic.AddInt32(&pushCount, 1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	var keychainCount int32
	var claudeCount int32
	mockExec := &MockCommandExecutor{
		RunFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "security" {
				atomic.AddInt32(&keychainCount, 1)
				return []byte(`{"claudeAiOauth":{"accessToken":"daemon-tok"}}`), nil
			}
			if name == "claude" {
				atomic.AddInt32(&claudeCount, 1)
				return []byte("keepalive output"), nil
			}
			return nil, errors.New("unexpected command")
		},
	}

	cfg := Config{
		PushURL:           server.URL,
		Interval:          5 * time.Millisecond,
		KeepAliveInterval: 5 * time.Millisecond,
		KeepAlive:         true,
	}

	daemon := NewDaemon(cfg, mockExec, http.DefaultClient)
	// Override pusher's backoffs for test safety
	daemon.pusher.backoffs = []time.Duration{0}

	ctx, cancel := context.WithCancel(context.Background())
	
	// Start daemon in background
	errChan := make(chan error, 1)
	go func() {
		errChan <- daemon.Start(ctx)
	}()

	// Wait 25ms to let tickers tick
	time.Sleep(25 * time.Millisecond)
	cancel()

	select {
	case err := <-errChan:
		if err != nil {
			t.Fatalf("daemon stopped with error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("daemon did not exit gracefully on context cancel")
	}

	if atomic.LoadInt32(&pushCount) == 0 {
		t.Error("expected daemon to perform at least 1 collect and push cycle")
	}
	if atomic.LoadInt32(&claudeCount) == 0 {
		t.Error("expected daemon to perform at least 1 keepalive cycle")
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test -v ./...`
Expected: FAIL with undefined: NewDaemon

- [x] **Step 3: Write minimal implementation**

Create `daemon.go`:
```go
package main

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"
)

type Daemon struct {
	cfg       Config
	exec      CommandExecutor
	collector *UsageCollector
	pusher    *Pusher
}

func NewDaemon(cfg Config, exec CommandExecutor, client *http.Client) *Daemon {
	if client == nil {
		client = &http.Client{}
	}
	return &Daemon{
		cfg:       cfg,
		exec:      exec,
		collector: NewUsageCollector(exec, client, ""),
		pusher:    NewPusher(client),
	}
}

func (d *Daemon) Start(ctx context.Context) error {
	var wg sync.WaitGroup

	// Routine 1: Collect & push loop
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("Daemon: starting usage collection loop (interval: %v)", d.cfg.Interval)

		// Initial load execution
		d.collectAndPush(ctx)

		ticker := time.NewTicker(d.cfg.Interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Println("Daemon: usage collection loop stopped")
				return
			case <-ticker.C:
				d.collectAndPush(ctx)
			}
		}
	}()

	// Routine 2: KeepAlive loop
	if d.cfg.KeepAlive {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("Daemon: starting keepalive loop (interval: %v)", d.cfg.KeepAliveInterval)

			// Initial keepalive execution
			d.runKeepAlive(ctx)

			ticker := time.NewTicker(d.cfg.KeepAliveInterval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					log.Println("Daemon: keepalive loop stopped")
					return
				case <-ticker.C:
					d.runKeepAlive(ctx)
				}
			}
		}()
	}

	wg.Wait()
	return nil
}

func (d *Daemon) collectAndPush(ctx context.Context) {
	start := time.Now()
	usage, err := d.collector.Collect(ctx)
	if err != nil {
		log.Printf("Daemon collect error: %v", err)
		return
	}

	err = d.pusher.Push(ctx, d.cfg.PushURL, usage)
	duration := time.Since(start)
	if err != nil {
		log.Printf("Daemon push error: %v (duration: %v)", err, duration)
		return
	}

	log.Printf("Daemon collected and pushed usage successfully in %v. Resets: [5h: %s, 7d: %s]",
		duration, usage.FiveHour.ResetsAt, usage.SevenDay.ResetsAt)
}

func (d *Daemon) runKeepAlive(ctx context.Context) {
	kam := NewKeepAliveManager(d.exec)
	if err := kam.KeepAlive(ctx); err != nil {
		log.Printf("Daemon keepalive failed: %v", err)
	} else {
		log.Println("Daemon keepalive triggered successfully")
	}
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test -v ./...`
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add daemon.go daemon_test.go
git commit -m "feat: implement Daemon ticker loops and concurrent scheduler routines"
```

---

### Task 7: Launchd Plist manager

**Files:**
- Create: `launchd.go`
- Create: `launchd_test.go`

- [x] **Step 1: Write the failing test**

Create `launchd_test.go`:
```go
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallAndUninstallDaemon(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "launchd_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	overridePlistPath = filepath.Join(tmpDir, "com.user.claude-usage-agent.plist")
	overrideLogPath = filepath.Join(tmpDir, "claude-usage-agent.log")
	defer func() {
		overridePlistPath = ""
		overrideLogPath = ""
	}()

	var commandsRun []string
	mockExec := &MockCommandExecutor{
		RunFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmdStr := name + " " + strings.Join(args, " ")
			commandsRun = append(commandsRun, cmdStr)
			return []byte("mock output"), nil
		},
	}

	cfg := Config{
		PushURL:           "http://localhost:1234/test",
		Interval:          10 * time.Minute,
		KeepAliveInterval: 2 * time.Hour,
		KeepAlive:         true,
	}

	// Test install
	err = InstallDaemon(context.Background(), cfg, mockExec)
	if err != nil {
		t.Fatalf("install daemon failed: %v", err)
	}

	// Verify file generated
	plistContent, err := os.ReadFile(overridePlistPath)
	if err != nil {
		t.Fatalf("failed to read generated plist: %v", err)
	}

	if !strings.Contains(string(plistContent), "<string>http://localhost:1234/test</string>") {
		t.Errorf("plist missing PushURL configuration, got content: %s", string(plistContent))
	}
	if !strings.Contains(string(plistContent), "<string>10m0s</string>") {
		t.Errorf("plist missing interval parameter, got content: %s", string(plistContent))
	}

	// Verify launchctl commands called
	if len(commandsRun) < 2 {
		t.Errorf("expected launchctl commands run, got: %v", commandsRun)
	}

	// Clear commands and test uninstall
	commandsRun = nil
	err = UninstallDaemon(context.Background(), mockExec)
	if err != nil {
		t.Fatalf("uninstall daemon failed: %v", err)
	}

	// Verify file deleted
	if _, err := os.Stat(overridePlistPath); !os.IsNotExist(err) {
		t.Error("expected plist file to be deleted upon uninstall")
	}

	if len(commandsRun) != 1 || !strings.Contains(commandsRun[0], "unload") {
		t.Errorf("expected launchctl unload cmd during uninstall, got: %v", commandsRun)
	}
}

func TestStatusDaemon(t *testing.T) {
	mockExec := &MockCommandExecutor{
		RunFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "launchctl" && args[0] == "print" {
				return []byte("pid = 4567\nstate = running\n"), nil
			}
			return nil, errors.New("unexpected command")
		},
	}

	status, err := StatusDaemon(context.Background(), mockExec)
	if err != nil {
		t.Fatalf("expected status to work, got error: %v", err)
	}
	if !strings.Contains(status, "pid = 4567") {
		t.Errorf("expected output to contain pid, got: %q", status)
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test -v ./...`
Expected: FAIL with undefined: InstallDaemon

- [x] **Step 3: Write minimal implementation**

Create `launchd.go`:
```go
package main

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"text/template"
)

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.user.claude-usage-agent</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.BinaryPath}}</string>
        <string>daemon</string>
        <string>--push-url</string>
        <string>{{.PushURL}}</string>
        <string>--interval</string>
        <string>{{.Interval}}</string>
        <string>--keepalive-interval</string>
        <string>{{.KeepAliveInterval}}</string>
        <string>--keepalive</string>
        <string>{{.KeepAlive}}</string>
    </array>
    <key>KeepAlive</key>
    <true/>
    <key>RunAtLoad</key>
    <true/>
    <key>StandardOutPath</key>
    <string>{{.LogPath}}</string>
    <key>StandardErrorPath</key>
    <string>{{.LogPath}}</string>
</dict>
</plist>`

type PlistConfig struct {
	BinaryPath        string
	PushURL           string
	Interval          string
	KeepAliveInterval string
	KeepAlive         string
	LogPath           string
}

var (
	overridePlistPath string
	overrideLogPath   string
)

func getPlistPath() (string, error) {
	if overridePlistPath != "" {
		return overridePlistPath, nil
	}
	usr, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(usr.HomeDir, "Library", "LaunchAgents", "com.user.claude-usage-agent.plist"), nil
}

func getLogPath() (string, error) {
	if overrideLogPath != "" {
		return overrideLogPath, nil
	}
	usr, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(usr.HomeDir, "Library", "Logs", "claude-usage-agent.log"), nil
}

func InstallDaemon(ctx context.Context, cfg Config, exec CommandExecutor) error {
	binaryPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get current executable path: %w", err)
	}

	plistPath, err := getPlistPath()
	if err != nil {
		return err
	}

	logPath, err := getLogPath()
	if err != nil {
		return err
	}

	// Ensure the parent directory exists
	if err := os.MkdirAll(filepath.Dir(plistPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory for plist: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory for logs: %w", err)
	}

	// If plist already loaded, unload first (ignore error)
	_, _ = exec.Run(ctx, "launchctl", "unload", plistPath)

	tmpl, err := template.New("plist").Parse(plistTemplate)
	if err != nil {
		return err
	}

	plistData := PlistConfig{
		BinaryPath:        binaryPath,
		PushURL:           cfg.PushURL,
		Interval:          cfg.Interval.String(),
		KeepAliveInterval: cfg.KeepAliveInterval.String(),
		KeepAlive:         fmt.Sprintf("%t", cfg.KeepAlive),
		LogPath:           logPath,
	}

	file, err := os.OpenFile(plistPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create/open plist file: %w", err)
	}
	defer file.Close()

	if err := tmpl.Execute(file, plistData); err != nil {
		return fmt.Errorf("failed to execute plist template: %w", err)
	}

	// launchctl load
	_, err = exec.Run(ctx, "launchctl", "load", plistPath)
	if err != nil {
		return fmt.Errorf("failed to load launchctl agent: %w", err)
	}

	return nil
}

func UninstallDaemon(ctx context.Context, exec CommandExecutor) error {
	plistPath, err := getPlistPath()
	if err != nil {
		return err
	}

	// launchctl unload
	_, _ = exec.Run(ctx, "launchctl", "unload", plistPath)

	// delete file
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove plist file: %w", err)
	}

	return nil
}

func StatusDaemon(ctx context.Context, exec CommandExecutor) (string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", err
	}
	uid := usr.Uid
	label := "com.user.claude-usage-agent"
	target := fmt.Sprintf("gui/%s/%s", uid, label)

	out, err := exec.Run(ctx, "launchctl", "print", target)
	if err != nil {
		return "Agent state: Unloaded or not running", nil
	}
	return string(out), nil
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test -v ./...`
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add launchd.go launchd_test.go
git commit -m "feat: implement InstallDaemon, UninstallDaemon, and StatusDaemon launchd managers"
```

---

### Task 8: CLI Commands Routing & main entry

**Files:**
- Create: `main.go`
- Create: `main_test.go`

- [x] **Step 1: Write the failing test**

Create `main_test.go`:
```go
package main

import (
	"os"
	"testing"
	"time"
)

func TestParseConfig_Defaults(t *testing.T) {
	os.Clearenv()
	cfg, remainingArgs, err := parseConfig([]string{})
	if err != nil {
		t.Fatalf("unexpected error parsing defaults: %v", err)
	}
	if cfg.PushURL != "http://localhost:8000/api/push/claude" {
		t.Errorf("expected default PushURL, got %q", cfg.PushURL)
	}
	if cfg.Interval != 5*time.Minute {
		t.Errorf("expected default interval 5m, got %v", cfg.Interval)
	}
	if cfg.KeepAliveInterval != 30*time.Minute {
		t.Errorf("expected default keepalive-interval 30m, got %v", cfg.KeepAliveInterval)
	}
	if !cfg.KeepAlive {
		t.Error("expected default keepalive to be true")
	}
	if len(remainingArgs) != 0 {
		t.Errorf("expected 0 remaining args, got %v", remainingArgs)
	}
}

func TestParseConfig_FlagsOverrideEnvAndDefaults(t *testing.T) {
	os.Clearenv()
	os.Setenv("CLAUDE_USAGE_PUSH_URL", "http://env-url.com")
	os.Setenv("CLAUDE_USAGE_INTERVAL", "1h")

	cfg, remainingArgs, err := parseConfig([]string{
		"--push-url", "http://flag-url.com",
		"--keepalive=false",
		"extra-argument",
	})
	if err != nil {
		t.Fatalf("unexpected parsing error: %v", err)
	}

	if cfg.PushURL != "http://flag-url.com" {
		t.Errorf("flags should override env vars. Expected 'http://flag-url.com', got %q", cfg.PushURL)
	}
	if cfg.Interval != 1*time.Hour {
		t.Errorf("env should override defaults. Expected 1h, got %v", cfg.Interval)
	}
	if cfg.KeepAlive {
		t.Error("flags should override defaults. Expected keepalive=false")
	}
	if len(remainingArgs) != 1 || remainingArgs[0] != "extra-argument" {
		t.Errorf("expected 1 remaining arg 'extra-argument', got %v", remainingArgs)
	}
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test -v ./...`
Expected: FAIL with undefined: parseConfig

- [x] **Step 3: Write minimal implementation**

Create `main.go`:
```go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

func parseConfig(args []string) (Config, []string, error) {
	fs := flag.NewFlagSet("claude-usage-agent", flag.ContinueOnError)

	pushURL := fs.String("push-url", getEnv("CLAUDE_USAGE_PUSH_URL", "http://localhost:8000/api/push/claude"), "Push destination URL")
	intervalStr := fs.String("interval", getEnv("CLAUDE_USAGE_INTERVAL", "5m"), "Collection interval")
	keepaliveStr := fs.String("keepalive-interval", getEnv("CLAUDE_USAGE_KEEPALIVE_INTERVAL", "30m"), "Keepalive interval")
	keepalive := fs.Bool("keepalive", getEnv("CLAUDE_USAGE_KEEPALIVE", "true") == "true", "Enable keepalive")

	err := fs.Parse(args)
	if err != nil {
		return Config{}, nil, err
	}

	interval, err := time.ParseDuration(*intervalStr)
	if err != nil {
		return Config{}, nil, fmt.Errorf("invalid interval: %w", err)
	}

	keepaliveInterval, err := time.ParseDuration(*keepaliveStr)
	if err != nil {
		return Config{}, nil, fmt.Errorf("invalid keepalive-interval: %w", err)
	}

	cfg := Config{
		PushURL:           *pushURL,
		Interval:          interval,
		KeepAliveInterval: keepaliveInterval,
		KeepAlive:         *keepalive,
	}

	return cfg, fs.Args(), nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: claude-usage-agent <command> [flags]")
		fmt.Println("Commands: daemon, collect, install, uninstall, status")
		os.Exit(1)
	}

	cmd := os.Args[1]
	cfg, _, err := parseConfig(os.Args[2:])
	if err != nil {
		log.Fatalf("Error parsing configurations: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	executor := &OSCommandExecutor{}
	client := &http.Client{Timeout: 10 * time.Second}

	switch cmd {
	case "daemon":
		daemon := NewDaemon(cfg, executor, client)
		if err := daemon.Start(ctx); err != nil {
			log.Fatalf("Daemon error: %v", err)
		}
	case "collect":
		collector := NewUsageCollector(executor, client, "")
		usage, err := collector.Collect(ctx)
		if err != nil {
			log.Fatalf("Collect error: %v", err)
		}
		pusher := NewPusher(client)
		if err := pusher.Push(ctx, cfg.PushURL, usage); err != nil {
			log.Fatalf("Push error: %v", err)
		}
		fmt.Println("Usage collected and pushed successfully")
	case "install":
		if err := InstallDaemon(ctx, cfg, executor); err != nil {
			log.Fatalf("Install error: %v", err)
		}
		fmt.Println("Daemon installed and loaded into launchd successfully")
	case "uninstall":
		if err := UninstallDaemon(ctx, executor); err != nil {
			log.Fatalf("Uninstall error: %v", err)
		}
		fmt.Println("Daemon uninstalled and unloaded from launchd successfully")
	case "status":
		status, err := StatusDaemon(ctx, executor)
		if err != nil {
			log.Fatalf("Status check error: %v", err)
		}
		fmt.Println(status)
	default:
		fmt.Printf("Unknown command: %s\n", cmd)
		os.Exit(1)
	}
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test -v ./...`
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: implement CLI subcommand routing and entrypoint flags"
```

---

### Task 9: Makefiles, README, Integration checks

**Files:**
- Create: `Makefile`
- Create: `README.md`

- [x] **Step 1: Write integration check test commands**

We will write build rules in `Makefile` and use a manual execution script to verify components.

- [x] **Step 2: Run test to verify it fails**

Run: `make build`
Expected: FAIL with no Makefile found

- [x] **Step 3: Write minimal implementation**

Create `Makefile`:
```makefile
.PHONY: build test install uninstall status clean

build:
	go build -o claude-usage-agent .

test:
	go test -v ./...

install: build
	./claude-usage-agent install

uninstall:
	./claude-usage-agent uninstall

status:
	./claude-usage-agent status

clean:
	rm -f claude-usage-agent
```

Create `README.md`:
```markdown
# Claude Usage Agent

A native macOS daemon utility in Go that fetches usage details from Anthropic OAuth API and sends telemetry to a configured HTTP endpoint.

## Subcommands

- `daemon`: Periodically fetches (5m) and keeps token fresh via Haiku call (30m).
- `collect`: Single collection run and push.
- `install`: Creates a launchd agent standard configuration and registers it.
- `uninstall`: Unregisters and removes plist configuration.
- `status`: Inspects active daemon status.
```

- [x] **Step 4: Run test to verify it passes**

Run: `make test && make build`
Expected: PASS with compiled `claude-usage-agent` binary

- [x] **Step 5: Commit**

```bash
git add Makefile README.md
git commit -m "feat: add Makefile build automation and project documentation"
```
