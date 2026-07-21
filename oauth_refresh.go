package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// claudeOAuthClientID is Claude Code's public OAuth client ID, sent with the
// refresh_token grant. It is not a secret — it's the same fixed value the CLI
// itself uses.
const claudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// defaultOAuthTokenURLs are the OAuth token endpoints tried in order. Anthropic
// migrated the endpoint from console.anthropic.com to platform.claude.com; the
// newer host is tried first, with the old one kept as a fallback.
var defaultOAuthTokenURLs = []string{
	"https://platform.claude.com/v1/oauth/token",
	"https://console.anthropic.com/v1/oauth/token",
}

// oauthRefresher rotates the OAuth token by exchanging the stored refresh token
// directly against Anthropic's token endpoint — the same standard OAuth refresh
// the CLI performs internally. Unlike `claude update`, this actually mints a new
// access token, and unlike a model prompt it spends no subscription quota.
type oauthRefresher struct {
	credPath  string
	tokenURLs []string
	client    *http.Client
	clientID  string
	now       func() time.Time
}

// newOAuthRefresher wires the production refresher against the real credentials
// file and Anthropic's token endpoints.
func newOAuthRefresher() *oauthRefresher {
	return &oauthRefresher{
		credPath:  defaultCredentialsPath(),
		tokenURLs: defaultOAuthTokenURLs,
		client:    &http.Client{Timeout: 30 * time.Second},
		clientID:  claudeOAuthClientID,
		now:       time.Now,
	}
}

// oauthTokenResponse is the subset of the token endpoint's JSON we consume.
// RefreshToken may be empty when the server rotates only the access token.
type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (o *oauthRefresher) Refresh(ctx context.Context) error {
	raw, err := os.ReadFile(o.credPath)
	if err != nil {
		return fmt.Errorf("read credentials file: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse credentials JSON: %w", err)
	}
	oauth, _ := doc["claudeAiOauth"].(map[string]any)
	if oauth == nil {
		return errors.New("credentials file has no claudeAiOauth object")
	}
	refreshToken, _ := oauth["refreshToken"].(string)
	if refreshToken == "" {
		return errors.New("credentials file has no refresh token")
	}

	tok, err := o.exchange(ctx, refreshToken)
	if err != nil {
		return err
	}

	oauth["accessToken"] = tok.AccessToken
	if tok.RefreshToken != "" {
		oauth["refreshToken"] = tok.RefreshToken
	}
	oauth["expiresAt"] = o.now().Add(time.Duration(tok.ExpiresIn) * time.Second).UnixMilli()

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials JSON: %w", err)
	}
	if err := os.WriteFile(o.credPath, out, 0o600); err != nil {
		return fmt.Errorf("write credentials file: %w", err)
	}
	return nil
}

// exchange POSTs the refresh_token grant to each endpoint in turn, returning the
// first successful response. All endpoint errors are joined so the log shows why
// each host was rejected.
func (o *oauthRefresher) exchange(ctx context.Context, refreshToken string) (oauthTokenResponse, error) {
	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     o.clientID,
	})
	if err != nil {
		return oauthTokenResponse{}, err
	}

	var errs []error
	for _, url := range o.tokenURLs {
		tok, err := o.post(ctx, url, body)
		if err == nil {
			return tok, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", url, err))
	}
	return oauthTokenResponse{}, fmt.Errorf("oauth refresh failed: %w", errors.Join(errs...))
}

func (o *oauthRefresher) post(ctx context.Context, url string, body []byte) (oauthTokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return oauthTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", claudeUserAgent())

	resp, err := o.client.Do(req)
	if err != nil {
		return oauthTokenResponse{}, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return oauthTokenResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return oauthTokenResponse{}, fmt.Errorf("status %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}
	var tok oauthTokenResponse
	if err := json.Unmarshal(respBody, &tok); err != nil {
		return oauthTokenResponse{}, fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" {
		return oauthTokenResponse{}, errors.New("token response missing access_token")
	}
	return tok, nil
}

// chainRefresher tries each refresher in order, returning as soon as one
// succeeds. Only when every refresher fails are their errors joined and
// returned. Ordering encodes preference (direct OAuth first, `claude update` as
// a fallback).
type chainRefresher struct {
	refreshers []TokenRefresher
}

func newChainRefresher(refreshers ...TokenRefresher) TokenRefresher {
	return &chainRefresher{refreshers: refreshers}
}

func (c *chainRefresher) Refresh(ctx context.Context) error {
	var errs []error
	for _, r := range c.refreshers {
		if err := r.Refresh(ctx); err == nil {
			return nil
		} else {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
