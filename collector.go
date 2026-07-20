package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// UsageAPIURL is the Anthropic usage endpoint. Overridable in tests via Collector.APIURL.
const UsageAPIURL = "https://api.anthropic.com/api/oauth/usage"

// anthropicBetaHeader matches the beta tag used by Claude Code itself.
const anthropicBetaHeader = "oauth-2025-04-20"

// KeychainService is the macOS Keychain generic-password service name under
// which Claude Code stores its credentials.
const KeychainService = "Claude Code-credentials"

// ErrNoToken is returned when the OAuth token cannot be read from Keychain.
var ErrNoToken = errors.New("cannot read OAuth token from Keychain")

// ErrRateLimit is the sentinel matched by errors.Is for 429 responses.
var ErrRateLimit = errors.New("rate limited")

// rateLimitError carries the server's Retry-After delay alongside the
// ErrRateLimit identity (zero when the header was absent or unparseable).
type rateLimitError struct {
	retryAfter time.Duration
}

func (e *rateLimitError) Error() string {
	if e.retryAfter > 0 {
		return fmt.Sprintf("rate limited (retry after %s)", e.retryAfter)
	}
	return "rate limited"
}

func (e *rateLimitError) Is(target error) bool { return target == ErrRateLimit }

// retryAfterFrom extracts the server-requested delay from a Collect error, or
// 0 when the error is not rate-limit related or carried no header.
func retryAfterFrom(err error) time.Duration {
	var rl *rateLimitError
	if errors.As(err, &rl) {
		return rl.retryAfter
	}
	return 0
}

// parseRetryAfter reads the Retry-After header as integer seconds. The
// HTTP-date form is rare on this endpoint and ignored.
func parseRetryAfter(header http.Header) time.Duration {
	if header == nil {
		return 0
	}
	raw := header.Get("Retry-After")
	if raw == "" {
		return 0
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// TokenProvider reads the raw Claude Code credentials JSON out of macOS Keychain.
// Abstracted as an interface so tests can inject a stub instead of shelling out
// to the real `security` command.
type TokenProvider interface {
	// Credentials returns the raw JSON blob stored in Keychain for Claude Code.
	Credentials() ([]byte, error)
}

// TokenRefresher runs a no-op Claude Code invocation so the CLI refreshes and
// persists a new OAuth token to Keychain. Abstracted for test stubbing.
type TokenRefresher interface {
	Refresh(ctx context.Context) error
}

// UsageAPI is the minimal HTTP client surface the collector needs. Abstracted
// so tests can point it at an httptest.Server.
type UsageAPI interface {
	// Fetch calls the usage endpoint with the given bearer token. The int is
	// the HTTP status code; the body and headers are returned raw on any status.
	Fetch(ctx context.Context, token string) (status int, body []byte, header http.Header, err error)
}

// usageAPI is the default production implementation backed by http.Client.
type usageAPI struct {
	url       string
	userAgent string
	client    *http.Client
}

func newUsageAPI(url string) UsageAPI {
	if url == "" {
		url = UsageAPIURL
	}
	return &usageAPI{
		url:       url,
		userAgent: claudeUserAgent(),
		client:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (u *usageAPI) Fetch(ctx context.Context, token string) (int, []byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.url, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", anthropicBetaHeader)
	req.Header.Set("User-Agent", u.userAgent)

	resp, err := u.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, resp.Header, err
	}
	return resp.StatusCode, body, resp.Header, nil
}

// Collector orchestrates: read token → call Usage API → refresh on 401 → retry
// once → produce a push-ready Payload. Its only cross-call state is the
// auth-failure guard below; it is not safe for concurrent Collect calls (the
// daemon drives it from a single loop).
type Collector struct {
	Tokens    TokenProvider
	Refresher TokenRefresher
	API       UsageAPI

	// lastFailedToken remembers a token that was still unauthorized after a
	// refresh. While the credentials source keeps returning it, cycles are
	// skipped without touching the API — retrying a known-bad token is what
	// provokes upstream 429s. Cleared when a different token shows up or a
	// cycle succeeds.
	lastFailedToken string
}

// apiResponse mirrors the shape returned by the usage endpoint. Only the
// fields we forward are decoded; everything else is ignored.
type apiResponse struct {
	SevenDay *struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    string  `json:"resets_at"`
	} `json:"seven_day"`
	FiveHour *struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    string  `json:"resets_at"`
	} `json:"five_hour"`
	// Limits carries per-scope caps. The model-scoped weekly windows (e.g. the
	// Fable limit) live here rather than as top-level fields.
	Limits []apiLimit `json:"limits"`
}

// apiLimit is one entry of the usage endpoint's limits list. A model-scoped
// weekly cap has kind "weekly_scoped" with scope.model.display_name naming the
// model (e.g. "Fable").
type apiLimit struct {
	Kind     string  `json:"kind"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

// fableModel is the display_name (substring) of the Fable model-scoped weekly
// limit we forward. Matched as a substring so a rename like "Fable 5" still
// resolves.
const fableModel = "Fable"

// findFableLimit returns the Fable weekly-scoped window as a quota, or nil when
// the account has no such cap. It stays optional — its absence never fails a
// cycle, unlike the required seven_day/five_hour windows.
func findFableLimit(limits []apiLimit) *quota {
	for _, l := range limits {
		if l.Kind != "weekly_scoped" || l.Scope == nil || l.Scope.Model == nil {
			continue
		}
		if strings.Contains(l.Scope.Model.DisplayName, fableModel) {
			return &quota{Utilization: l.Percent, ResetsAt: l.ResetsAt}
		}
	}
	return nil
}

// Payload is the exact shape POSTed to the push URL. The ai-plan-insight v2
// ClaudePushRequest schema requires both seven_day and five_hour, so neither
// field is optional here — Collect fails instead of pushing a partial payload
// the server would reject with 422.
type Payload struct {
	SevenDay quota `json:"seven_day"`
	FiveHour quota `json:"five_hour"`
	// Fable is the model-scoped weekly window. Optional: omitted (nil) when the
	// account has no Fable cap, so the server's optional `fable` field stays
	// unset rather than receiving a zero-valued window.
	Fable *quota `json:"fable,omitempty"`
}

type quota struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

// expiryLeeway is how close to expiresAt a token is already treated as
// expired, mirroring the CLI's own ~60s proactive-refresh window.
const expiryLeeway = 60 * time.Second

// oauthCreds is the decoded token material from the credentials blob.
// ExpiresAt is zero when the blob carries no expiry (older formats) — such a
// token is treated as fresh and any staleness surfaces as a 401 instead.
type oauthCreds struct {
	Token     string
	ExpiresAt time.Time
}

// accessToken extracts the OAuth access token and its expiry from the
// credentials blob. expiresAt is stored as epoch milliseconds by Claude Code;
// epoch seconds are accepted too for robustness.
func accessToken(raw []byte) (oauthCreds, error) {
	var doc struct {
		ClaudeAiOauth struct {
			AccessToken string  `json:"accessToken"`
			ExpiresAt   float64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return oauthCreds{}, fmt.Errorf("parse credentials JSON: %w", err)
	}
	if doc.ClaudeAiOauth.AccessToken == "" {
		return oauthCreds{}, ErrNoToken
	}
	creds := oauthCreds{Token: doc.ClaudeAiOauth.AccessToken}
	if ts := doc.ClaudeAiOauth.ExpiresAt; ts > 0 {
		if ts > 1e12 { // milliseconds
			creds.ExpiresAt = time.UnixMilli(int64(ts))
		} else { // seconds
			creds.ExpiresAt = time.Unix(int64(ts), 0)
		}
	}
	return creds, nil
}

// expired reports whether the token should be refreshed before use.
func (c oauthCreds) expired(now time.Time) bool {
	return !c.ExpiresAt.IsZero() && now.After(c.ExpiresAt.Add(-expiryLeeway))
}

// Collect performs one full collection cycle:
//  1. read token (+ expiry) from the credentials source
//  2. if expiresAt says the token is stale, refresh via the CLI first
//  3. call the usage API
//  4. on 401, refresh the token via the Claude CLI and retry the API once
//  5. decode the response into a Payload
//
// Returns the ops performed (mirrors the reference project's logging style)
// and either the Payload or an error describing where the cycle failed.
func (c *Collector) Collect(ctx context.Context) (payload *Payload, ops []string, err error) {
	ops = []string{}

	creds, err := c.readToken()
	if err != nil {
		return nil, ops, err
	}
	if c.lastFailedToken != "" && creds.Token == c.lastFailedToken {
		return nil, ops, errors.New("auth failed previously and token is unchanged; waiting for the CLI to rotate credentials")
	}

	// Local expiry check: never spend a request on a token we already know is
	// stale. If the refresh doesn't rotate it (CLI idle, update a no-op), arm
	// the guard so we go quiet until new credentials appear.
	if creds.expired(time.Now()) {
		ops = append(ops, "refresh-token")
		log.Printf("collector: token expired locally (expiresAt=%s), refreshing", creds.ExpiresAt.Format(time.RFC3339))
		if rerr := c.Refresher.Refresh(ctx); rerr != nil {
			return nil, ops, fmt.Errorf("refresh token: %w", rerr)
		}
		creds, err = c.readToken()
		if err != nil {
			return nil, ops, err
		}
		if creds.expired(time.Now()) {
			c.lastFailedToken = creds.Token
			return nil, ops, errors.New("token still expired after refresh; waiting for the CLI to rotate credentials")
		}
	}
	token := creds.Token

	// First API attempt. A transport error yields status 0 + non-nil callErr;
	// guard against it before the 401 branch so a 401 refresh is only ever
	// attempted when we actually received an HTTP response.
	status, body, header, callErr := c.API.Fetch(ctx, token)
	if callErr != nil && status != http.StatusUnauthorized {
		ops = append(ops, "api-failed")
		return nil, ops, fmt.Errorf("usage api: %w", callErr)
	}
	if status == http.StatusUnauthorized {
		ops = append(ops, "refresh-token")
		log.Printf("collector: token expired (401), refreshing")
		if rerr := c.Refresher.Refresh(ctx); rerr != nil {
			return nil, ops, fmt.Errorf("refresh token: %w", rerr)
		}
		creds, err = c.readToken()
		if err != nil {
			return nil, ops, err
		}
		token = creds.Token
		status, body, header, callErr = c.API.Fetch(ctx, token)
	}
	if callErr != nil {
		ops = append(ops, "api-failed")
		return nil, ops, fmt.Errorf("usage api: %w", callErr)
	}
	if status == http.StatusUnauthorized {
		ops = append(ops, "api-failed")
		c.lastFailedToken = token
		return nil, ops, errors.New("usage api: still unauthorized after refresh")
	}
	if status == http.StatusTooManyRequests {
		ops = append(ops, "api-failed")
		return nil, ops, fmt.Errorf("usage api: %w", &rateLimitError{retryAfter: parseRetryAfter(header)})
	}
	if status < 200 || status >= 300 {
		ops = append(ops, "api-failed")
		return nil, ops, fmt.Errorf("usage api: unexpected status %d: %s", status, strings.TrimSpace(string(body)))
	}

	var apiResp apiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		ops = append(ops, "api-failed")
		return nil, ops, fmt.Errorf("decode usage response: %w", err)
	}

	if apiResp.SevenDay == nil || apiResp.FiveHour == nil {
		ops = append(ops, "api-failed")
		return nil, ops, errors.New("usage response missing seven_day or five_hour (both required by the v2 push API)")
	}

	ops = append(ops, "api-ok")
	c.lastFailedToken = ""
	payload = &Payload{
		SevenDay: quota{Utilization: apiResp.SevenDay.Utilization, ResetsAt: apiResp.SevenDay.ResetsAt},
		FiveHour: quota{Utilization: apiResp.FiveHour.Utilization, ResetsAt: apiResp.FiveHour.ResetsAt},
	}
	if fable := findFableLimit(apiResp.Limits); fable != nil {
		payload.Fable = fable
		ops = append(ops, "fable-ok")
	}
	return payload, ops, nil
}

func (c *Collector) readToken() (oauthCreds, error) {
	raw, err := c.Tokens.Credentials()
	if err != nil {
		return oauthCreds{}, fmt.Errorf("%w: %v", ErrNoToken, err)
	}
	return accessToken(raw)
}
