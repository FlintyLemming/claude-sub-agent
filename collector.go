package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
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
	// the HTTP status code; the body is returned raw on any status.
	Fetch(ctx context.Context, token string) (status int, body []byte, err error)
}

// usageAPI is the default production implementation backed by http.Client.
type usageAPI struct {
	url    string
	client *http.Client
}

func newUsageAPI(url string) UsageAPI {
	if url == "" {
		url = UsageAPIURL
	}
	return &usageAPI{
		url:    url,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (u *usageAPI) Fetch(ctx context.Context, token string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", anthropicBetaHeader)

	resp, err := u.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// Collector orchestrates: read token → call Usage API → refresh on 401 → retry
// once → produce a push-ready Payload. It owns no state between calls.
type Collector struct {
	Tokens    TokenProvider
	Refresher TokenRefresher
	API       UsageAPI
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
}

// Payload is the exact shape POSTed to the push URL. See the design doc §4:
// only seven_day and five_hour utilization + resets_at are forwarded.
type Payload struct {
	SevenDay *quota `json:"seven_day,omitempty"`
	FiveHour *quota `json:"five_hour,omitempty"`
}

type quota struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

// accessToken extracts the OAuth access token from the Keychain credentials blob.
func accessToken(raw []byte) (string, error) {
	var doc struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("parse credentials JSON: %w", err)
	}
	if doc.ClaudeAiOauth.AccessToken == "" {
		return "", ErrNoToken
	}
	return doc.ClaudeAiOauth.AccessToken, nil
}

// Collect performs one full collection cycle:
//  1. read token from Keychain
//  2. call the usage API
//  3. on 401, refresh the token via the Claude CLI and retry the API once
//  4. decode the response into a Payload
//
// Returns the ops performed (mirrors the reference project's logging style)
// and either the Payload or an error describing where the cycle failed.
func (c *Collector) Collect(ctx context.Context) (payload *Payload, ops []string, err error) {
	ops = []string{}

	token, err := c.readToken()
	if err != nil {
		return nil, ops, err
	}

	// First API attempt. A transport error yields status 0 + non-nil callErr;
	// guard against it before the 401 branch so a 401 refresh is only ever
	// attempted when we actually received an HTTP response.
	status, body, callErr := c.API.Fetch(ctx, token)
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
		token, err = c.readToken()
		if err != nil {
			return nil, ops, err
		}
		status, body, callErr = c.API.Fetch(ctx, token)
	}
	if callErr != nil {
		ops = append(ops, "api-failed")
		return nil, ops, fmt.Errorf("usage api: %w", callErr)
	}
	if status == http.StatusUnauthorized {
		ops = append(ops, "api-failed")
		return nil, ops, errors.New("usage api: still unauthorized after refresh")
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

	ops = append(ops, "api-ok")
	return &Payload{
		SevenDay: toQuota(apiResp.SevenDay),
		FiveHour: toQuota(apiResp.FiveHour),
	}, ops, nil
}

func (c *Collector) readToken() (string, error) {
	raw, err := c.Tokens.Credentials()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoToken, err)
	}
	return accessToken(raw)
}

func toQuota(in *struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}) *quota {
	if in == nil {
		return nil
	}
	return &quota{Utilization: in.Utilization, ResetsAt: in.ResetsAt}
}
