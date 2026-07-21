package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeCreds writes a credentials.json with the given oauth sub-object and
// returns its path.
func writeCreds(t *testing.T, oauth map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")
	blob, err := json.Marshal(map[string]any{"claudeAiOauth": oauth})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// readOAuth reads back the claudeAiOauth object from a credentials file.
func readOAuth(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ClaudeAiOauth map[string]any `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.ClaudeAiOauth
}

// TestOAuthRefresher_WritesNewTokens is the core happy path: given a stale token
// with a valid refresh token, Refresh POSTs the standard OAuth refresh_token
// grant and persists the returned tokens (and a fresh expiresAt) back to the
// credentials file, preserving unrelated fields.
func TestOAuthRefresher_WritesNewTokens(t *testing.T) {
	credPath := writeCreds(t, map[string]any{
		"accessToken":      "old-access",
		"refreshToken":     "old-refresh",
		"expiresAt":        float64(time.Now().Add(-time.Hour).UnixMilli()),
		"scopes":           []any{"user:inference", "user:profile"},
		"subscriptionType": "pro",
	})

	var gotBody map[string]any
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    28800,
			"token_type":    "Bearer",
		})
	}))
	defer srv.Close()

	fixedNow := time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC)
	r := &oauthRefresher{
		credPath:  credPath,
		tokenURLs: []string{srv.URL},
		client:    srv.Client(),
		clientID:  claudeOAuthClientID,
		now:       func() time.Time { return fixedNow },
	}

	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh err = %v", err)
	}

	// Request shape.
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody["grant_type"] != "refresh_token" {
		t.Errorf("grant_type = %v, want refresh_token", gotBody["grant_type"])
	}
	if gotBody["refresh_token"] != "old-refresh" {
		t.Errorf("refresh_token = %v, want old-refresh", gotBody["refresh_token"])
	}
	if gotBody["client_id"] != claudeOAuthClientID {
		t.Errorf("client_id = %v, want %s", gotBody["client_id"], claudeOAuthClientID)
	}

	// Persisted result.
	oauth := readOAuth(t, credPath)
	if oauth["accessToken"] != "new-access" {
		t.Errorf("accessToken = %v, want new-access", oauth["accessToken"])
	}
	if oauth["refreshToken"] != "new-refresh" {
		t.Errorf("refreshToken = %v, want new-refresh", oauth["refreshToken"])
	}
	wantExp := float64(fixedNow.Add(28800 * time.Second).UnixMilli())
	if oauth["expiresAt"] != wantExp {
		t.Errorf("expiresAt = %v, want %v", oauth["expiresAt"], wantExp)
	}
	// Unrelated fields preserved.
	if oauth["subscriptionType"] != "pro" {
		t.Errorf("subscriptionType = %v, want pro (preserved)", oauth["subscriptionType"])
	}
	if scopes, ok := oauth["scopes"].([]any); !ok || len(scopes) != 2 {
		t.Errorf("scopes = %v, want the 2 preserved scopes", oauth["scopes"])
	}
}

// TestOAuthRefresher_FallsBackToSecondURL: when the primary endpoint fails
// (Anthropic migrated hosts and the old one 404s), the refresher tries the next
// URL before giving up.
func TestOAuthRefresher_FallsBackToSecondURL(t *testing.T) {
	credPath := writeCreds(t, map[string]any{
		"accessToken":  "old-access",
		"refreshToken": "old-refresh",
		"expiresAt":    float64(time.Now().Add(-time.Hour).UnixMilli()),
	})

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    28800,
		})
	}))
	defer secondary.Close()

	r := &oauthRefresher{
		credPath:  credPath,
		tokenURLs: []string{primary.URL, secondary.URL},
		client:    primary.Client(),
		clientID:  claudeOAuthClientID,
		now:       time.Now,
	}
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh err = %v", err)
	}
	if oauth := readOAuth(t, credPath); oauth["accessToken"] != "new-access" {
		t.Errorf("accessToken = %v, want new-access (from fallback URL)", oauth["accessToken"])
	}
}

// TestOAuthRefresher_MissingRefreshToken: with no refresh token on disk there's
// nothing to exchange, so Refresh errors (and the caller can fall back to the
// CLI refresher). No HTTP call should be made.
func TestOAuthRefresher_MissingRefreshToken(t *testing.T) {
	credPath := writeCreds(t, map[string]any{
		"accessToken": "old-access",
		"expiresAt":   float64(time.Now().Add(-time.Hour).UnixMilli()),
	})
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	r := &oauthRefresher{
		credPath:  credPath,
		tokenURLs: []string{srv.URL},
		client:    srv.Client(),
		clientID:  claudeOAuthClientID,
		now:       time.Now,
	}
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("expected error when refresh token is missing")
	}
	if called {
		t.Error("HTTP endpoint should not be called when no refresh token exists")
	}
}

// TestOAuthRefresher_AllEndpointsFail: every endpoint erroring yields an error
// and leaves the credentials file untouched (no half-written token).
func TestOAuthRefresher_AllEndpointsFail(t *testing.T) {
	credPath := writeCreds(t, map[string]any{
		"accessToken":  "old-access",
		"refreshToken": "old-refresh",
		"expiresAt":    float64(time.Now().Add(-time.Hour).UnixMilli()),
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := &oauthRefresher{
		credPath:  credPath,
		tokenURLs: []string{srv.URL, srv.URL},
		client:    srv.Client(),
		clientID:  claudeOAuthClientID,
		now:       time.Now,
	}
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("expected error when all endpoints fail")
	}
	if oauth := readOAuth(t, credPath); oauth["accessToken"] != "old-access" {
		t.Errorf("accessToken = %v, want old-access unchanged on failure", oauth["accessToken"])
	}
}

// TestOAuthRefresher_KeepsOldRefreshTokenWhenOmitted: some OAuth servers rotate
// only the access token and omit refresh_token in the response; the stored
// refresh token must survive so the next cycle can still refresh.
func TestOAuthRefresher_KeepsOldRefreshTokenWhenOmitted(t *testing.T) {
	credPath := writeCreds(t, map[string]any{
		"accessToken":  "old-access",
		"refreshToken": "old-refresh",
		"expiresAt":    float64(time.Now().Add(-time.Hour).UnixMilli()),
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-access",
			"expires_in":   28800,
		})
	}))
	defer srv.Close()

	r := &oauthRefresher{
		credPath:  credPath,
		tokenURLs: []string{srv.URL},
		client:    srv.Client(),
		clientID:  claudeOAuthClientID,
		now:       time.Now,
	}
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh err = %v", err)
	}
	oauth := readOAuth(t, credPath)
	if oauth["accessToken"] != "new-access" {
		t.Errorf("accessToken = %v, want new-access", oauth["accessToken"])
	}
	if oauth["refreshToken"] != "old-refresh" {
		t.Errorf("refreshToken = %v, want old-refresh preserved", oauth["refreshToken"])
	}
}

// recordingRefresher notes whether it ran and returns a preset error.
type recordingRefresher struct {
	called *bool
	err    error
}

func (r recordingRefresher) Refresh(ctx context.Context) error {
	*r.called = true
	return r.err
}

func TestChainRefresher(t *testing.T) {
	t.Run("first succeeds, later ones not consulted", func(t *testing.T) {
		firstCalled, secondCalled := false, false
		chain := newChainRefresher(
			recordingRefresher{called: &firstCalled},
			recordingRefresher{called: &secondCalled, err: errors.New("nope")},
		)
		if err := chain.Refresh(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !firstCalled {
			t.Error("first refresher should have run")
		}
		if secondCalled {
			t.Error("second refresher should not run once the first succeeds")
		}
	})

	t.Run("falls back to second when first fails", func(t *testing.T) {
		firstCalled, secondCalled := false, false
		chain := newChainRefresher(
			recordingRefresher{called: &firstCalled, err: errors.New("oauth down")},
			recordingRefresher{called: &secondCalled},
		)
		if err := chain.Refresh(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !secondCalled {
			t.Error("second refresher should run when the first fails")
		}
	})

	t.Run("all fail, errors joined", func(t *testing.T) {
		a, b := errors.New("oauth down"), errors.New("cli missing")
		c1, c2 := false, false
		chain := newChainRefresher(
			recordingRefresher{called: &c1, err: a},
			recordingRefresher{called: &c2, err: b},
		)
		err := chain.Refresh(context.Background())
		if err == nil {
			t.Fatal("expected error when all refreshers fail")
		}
		if !errors.Is(err, a) || !errors.Is(err, b) {
			t.Fatalf("joined error missing a cause: %v", err)
		}
	})
}

// TestDefaultOAuthTokenURLs pins the endpoint order: Anthropic migrated the
// OAuth token endpoint to platform.claude.com, with console.anthropic.com kept
// as a fallback for older accounts/routes.
func TestDefaultOAuthTokenURLs(t *testing.T) {
	if len(defaultOAuthTokenURLs) < 2 {
		t.Fatalf("want at least two endpoints, got %v", defaultOAuthTokenURLs)
	}
	if !strings.Contains(defaultOAuthTokenURLs[0], "platform.claude.com") {
		t.Errorf("primary endpoint = %q, want platform.claude.com", defaultOAuthTokenURLs[0])
	}
	if !strings.Contains(defaultOAuthTokenURLs[1], "console.anthropic.com") {
		t.Errorf("fallback endpoint = %q, want console.anthropic.com", defaultOAuthTokenURLs[1])
	}
}
