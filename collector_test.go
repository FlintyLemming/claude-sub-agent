package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubTokens returns a fixed credentials blob. keychainFail makes Credentials
// return an error to simulate the second read after a failed refresh.
type stubTokens struct {
	cred        []byte
	keychainErr error
}

func (s *stubTokens) Credentials() ([]byte, error) {
	if s.keychainErr != nil {
		return nil, s.keychainErr
	}
	return s.cred, nil
}

// stubRefresher records calls and optionally returns an error.
type stubRefresher struct {
	calls int32
	err   error
}

func (s *stubRefresher) Refresh(ctx context.Context) error {
	atomic.AddInt32(&s.calls, 1)
	return s.err
}

// credJSON builds a valid Keychain credentials blob for tests.
func credJSON(t *testing.T, token string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": token},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// credJSONExpiring builds a credentials blob carrying an expiresAt timestamp
// (milliseconds since epoch, as Claude Code stores it).
func credJSONExpiring(t *testing.T, token string, expiresAt time.Time) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken": token,
			"expiresAt":   expiresAt.UnixMilli(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// usageResp builds a usage API JSON body with the given utilization values.
func usageResp(t *testing.T, sevenUtil float64, sevenReset string, fiveUtil float64, fiveReset string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"seven_day": map[string]any{"utilization": sevenUtil, "resets_at": sevenReset},
		"five_hour": map[string]any{"utilization": fiveUtil, "resets_at": fiveReset},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newHTTPUsageAPI points a usageAPI at a test server so Fetch goes over HTTP.
func newHTTPUsageAPI(t *testing.T, url string) UsageAPI {
	t.Helper()
	return newUsageAPI(url)
}

func TestCollector_Success(t *testing.T) {
	var apiCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&apiCalls, 1)
		if got := r.Header.Get("Authorization"); got != "Bearer first-token" {
			t.Errorf("auth header = %q, want Bearer first-token", got)
		}
		if got := r.Header.Get("anthropic-beta"); got != anthropicBetaHeader {
			t.Errorf("beta header = %q", got)
		}
		w.WriteHeader(http.StatusOK)
		w.Write(usageResp(t, 45.2, "2026-07-08T12:00:00Z", 12.8, "2026-07-01T15:00:00Z"))
	}))
	defer srv.Close()

	refresher := &stubRefresher{}
	c := &Collector{
		Tokens:    &stubTokens{cred: credJSON(t, "first-token")},
		Refresher: refresher,
		API:       newHTTPUsageAPI(t, srv.URL),
	}

	payload, ops, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect err = %v", err)
	}
	if got := atomic.LoadInt32(&apiCalls); got != 1 {
		t.Errorf("api calls = %d, want 1", got)
	}
	if refresher.calls != 0 {
		t.Errorf("refresher called %d times on success, want 0", refresher.calls)
	}
	if len(ops) != 1 || ops[0] != "api-ok" {
		t.Errorf("ops = %v, want [api-ok]", ops)
	}
	if payload.SevenDay.Utilization != 45.2 || payload.SevenDay.ResetsAt != "2026-07-08T12:00:00Z" {
		t.Errorf("seven_day payload = %+v", payload.SevenDay)
	}
	if payload.FiveHour.Utilization != 12.8 || payload.FiveHour.ResetsAt != "2026-07-01T15:00:00Z" {
		t.Errorf("five_hour payload = %+v", payload.FiveHour)
	}
}

func TestCollector_FableWindow(t *testing.T) {
	// Usage body with a weekly_scoped Fable limit alongside the required windows.
	body := []byte(`{
		"seven_day": {"utilization": 25.0, "resets_at": "2026-07-26T07:00:00Z"},
		"five_hour": {"utilization": 15.0, "resets_at": "2026-07-20T04:40:00Z"},
		"limits": [
			{"kind": "session", "percent": 15, "resets_at": "2026-07-20T04:40:00Z"},
			{"kind": "weekly_all", "percent": 25, "resets_at": "2026-07-26T07:00:00Z"},
			{"kind": "weekly_scoped", "percent": 44, "resets_at": "2026-07-26T07:00:00Z",
			 "scope": {"model": {"display_name": "Fable"}}}
		]
	}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	defer srv.Close()

	c := &Collector{
		Tokens:    &stubTokens{cred: credJSON(t, "tok")},
		Refresher: &stubRefresher{},
		API:       newHTTPUsageAPI(t, srv.URL),
	}

	payload, ops, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect err = %v", err)
	}
	if payload.Fable == nil {
		t.Fatal("payload.Fable = nil, want captured Fable window")
	}
	if payload.Fable.Utilization != 44 || payload.Fable.ResetsAt != "2026-07-26T07:00:00Z" {
		t.Errorf("fable payload = %+v", payload.Fable)
	}
	wantOps := []string{"api-ok", "fable-ok"}
	if len(ops) != 2 || ops[0] != wantOps[0] || ops[1] != wantOps[1] {
		t.Errorf("ops = %v, want %v", ops, wantOps)
	}
}

func TestCollector_NoFableWindow(t *testing.T) {
	// The plain usageResp helper has no limits list → Fable stays nil, no op.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(usageResp(t, 25.0, "2026-07-26T07:00:00Z", 15.0, "2026-07-20T04:40:00Z"))
	}))
	defer srv.Close()

	c := &Collector{
		Tokens:    &stubTokens{cred: credJSON(t, "tok")},
		Refresher: &stubRefresher{},
		API:       newHTTPUsageAPI(t, srv.URL),
	}

	payload, ops, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect err = %v", err)
	}
	if payload.Fable != nil {
		t.Errorf("payload.Fable = %+v, want nil when no Fable limit present", payload.Fable)
	}
	if len(ops) != 1 || ops[0] != "api-ok" {
		t.Errorf("ops = %v, want [api-ok]", ops)
	}
}

func TestCollector_401RefreshSuccess(t *testing.T) {
	var apiCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&apiCalls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer new" {
			t.Errorf("retry auth = %q, want Bearer new", got)
		}
		w.WriteHeader(http.StatusOK)
		w.Write(usageResp(t, 60.0, "2026-07-09T00:00:00Z", 5.0, "2026-07-01T16:00:00Z"))
	}))
	defer srv.Close()

	// updatingTokens returns the old token until Refresh runs, then the new
	// token — simulating the CLI rewriting the keychain.
	tok := &updatingTokens{inner: &stubTokens{cred: credJSON(t, "old")}, after: credJSON(t, "new")}
	refresher := &swapRefresher{inner: &stubRefresher{}, tok: tok}
	c := &Collector{Tokens: tok, Refresher: refresher, API: newHTTPUsageAPI(t, srv.URL)}

	payload, ops, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect err = %v", err)
	}
	if got := atomic.LoadInt32(&apiCalls); got != 2 {
		t.Errorf("api calls = %d, want 2", got)
	}
	if refresher.inner.calls != 1 {
		t.Errorf("refresher calls = %d, want 1", refresher.inner.calls)
	}
	if payload.SevenDay.Utilization != 60.0 {
		t.Errorf("payload util = %v, want 60", payload.SevenDay.Utilization)
	}
	wantOps := []string{"refresh-token", "api-ok"}
	if len(ops) != 2 || ops[0] != wantOps[0] || ops[1] != wantOps[1] {
		t.Errorf("ops = %v, want %v", ops, wantOps)
	}
}

func TestCollector_401RefreshStillUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // always 401
	}))
	defer srv.Close()

	refresher := &stubRefresher{}
	c := &Collector{
		Tokens:    &stubTokens{cred: credJSON(t, "bad-token")},
		Refresher: refresher,
		API:       newHTTPUsageAPI(t, srv.URL),
	}

	_, ops, err := c.Collect(context.Background())
	if err == nil {
		t.Fatal("Collect err = nil, want error")
	}
	if refresher.calls != 1 {
		t.Errorf("refresher calls = %d, want 1", refresher.calls)
	}
	// Should record refresh-token then api-failed.
	foundRefresh := false
	foundFailed := false
	for _, o := range ops {
		if o == "refresh-token" {
			foundRefresh = true
		}
		if o == "api-failed" {
			foundFailed = true
		}
	}
	if !foundRefresh || !foundFailed {
		t.Errorf("ops = %v, want both refresh-token and api-failed", ops)
	}
}

func TestCollector_RefreshError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	refresher := &stubRefresher{err: errors.New("claude not installed")}
	c := &Collector{
		Tokens:    &stubTokens{cred: credJSON(t, "expired")},
		Refresher: refresher,
		API:       newHTTPUsageAPI(t, srv.URL),
	}

	_, ops, err := c.Collect(context.Background())
	if err == nil {
		t.Fatal("Collect err = nil, want refresh error")
	}
	if refresher.calls != 1 {
		t.Errorf("refresher calls = %d, want 1", refresher.calls)
	}
	if !contains(ops, "refresh-token") {
		t.Errorf("ops = %v, want refresh-token", ops)
	}
}

func TestCollector_NetworkError(t *testing.T) {
	// A server that immediately closes connections: point the client at an
	// already-closed server to force a transport error on every call.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	c := &Collector{
		Tokens:    &stubTokens{cred: credJSON(t, "some-token")},
		Refresher: &stubRefresher{},
		API:       newHTTPUsageAPI(t, srv.URL),
	}

	_, ops, err := c.Collect(context.Background())
	if err == nil {
		t.Fatal("Collect err = nil, want network error")
	}
	if !contains(ops, "api-failed") {
		t.Errorf("ops = %v, want api-failed", ops)
	}
}

func TestCollector_NoTokenInKeychain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("api should not be called when token is missing")
	}))
	defer srv.Close()

	c := &Collector{
		Tokens:    &stubTokens{cred: []byte(`{"claudeAiOauth":{}}`)},
		Refresher: &stubRefresher{},
		API:       newHTTPUsageAPI(t, srv.URL),
	}

	_, _, err := c.Collect(context.Background())
	if !errors.Is(err, ErrNoToken) {
		t.Errorf("err = %v, want ErrNoToken", err)
	}
}

func TestCollector_MissingWindowRejected(t *testing.T) {
	// v2 ClaudePushRequest requires both seven_day and five_hour; a response
	// missing either must fail collection instead of producing a partial payload.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"seven_day": {"utilization": 10, "resets_at": "2026-07-08T12:00:00Z"}}`))
	}))
	defer srv.Close()

	c := &Collector{
		Tokens:    &stubTokens{cred: credJSON(t, "tok")},
		Refresher: &stubRefresher{},
		API:       newHTTPUsageAPI(t, srv.URL),
	}

	_, ops, err := c.Collect(context.Background())
	if err == nil {
		t.Fatal("Collect err = nil, want error for missing five_hour")
	}
	if !contains(ops, "api-failed") {
		t.Errorf("ops = %v, want api-failed", ops)
	}
}

// TestCollector_SkipsWhenTokenUnchangedAfterAuthFailure: after a cycle ends
// "still unauthorized after refresh", further cycles must not hit the API (or
// re-run the refresher) until the on-disk token actually changes — hammering
// with a known-bad token is what triggers upstream 429s.
func TestCollector_SkipsWhenTokenUnchangedAfterAuthFailure(t *testing.T) {
	var apiCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&apiCalls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	refresher := &stubRefresher{}
	c := &Collector{
		Tokens:    &stubTokens{cred: credJSON(t, "bad-token")},
		Refresher: refresher,
		API:       newHTTPUsageAPI(t, srv.URL),
	}

	if _, _, err := c.Collect(context.Background()); err == nil {
		t.Fatal("first Collect err = nil, want auth failure")
	}
	callsAfterFirst := atomic.LoadInt32(&apiCalls)

	_, ops, err := c.Collect(context.Background())
	if err == nil {
		t.Fatal("second Collect err = nil, want skip error while token unchanged")
	}
	if got := atomic.LoadInt32(&apiCalls); got != callsAfterFirst {
		t.Errorf("api calls %d -> %d; want no new calls while token unchanged", callsAfterFirst, got)
	}
	if refresher.calls != 1 {
		t.Errorf("refresher calls = %d, want 1 (no re-refresh while token unchanged)", refresher.calls)
	}
	if len(ops) != 0 {
		t.Errorf("ops = %v, want none for a skipped cycle", ops)
	}
}

// TestCollector_ResumesWhenTokenChanges: the auth-failure guard must clear as
// soon as a different token appears in the credentials source (the CLI rotated
// it), letting the next cycle collect normally.
func TestCollector_ResumesWhenTokenChanges(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(usageResp(t, 30.0, "2026-07-26T07:00:00Z", 10.0, "2026-07-20T12:00:00Z"))
	}))
	defer srv.Close()

	tok := &stubTokens{cred: credJSON(t, "bad")}
	c := &Collector{
		Tokens:    tok,
		Refresher: &stubRefresher{},
		API:       newHTTPUsageAPI(t, srv.URL),
	}

	if _, _, err := c.Collect(context.Background()); err == nil {
		t.Fatal("first Collect err = nil, want auth failure")
	}

	// CLI rotates the credentials → guard must lift.
	tok.cred = credJSON(t, "good")
	payload, ops, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect after token change err = %v", err)
	}
	if payload.SevenDay.Utilization != 30.0 {
		t.Errorf("payload = %+v", payload.SevenDay)
	}
	if !contains(ops, "api-ok") {
		t.Errorf("ops = %v, want api-ok", ops)
	}
}

// TestCollector_RefreshesLocallyExpiredTokenBeforeAPI: when expiresAt says the
// token is already (or nearly) expired, the collector must refresh first and
// never send the doomed request — the API only ever sees the rotated token.
func TestCollector_RefreshesLocallyExpiredTokenBeforeAPI(t *testing.T) {
	var apiCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&apiCalls, 1)
		if got := r.Header.Get("Authorization"); got != "Bearer fresh" {
			t.Errorf("auth header = %q, want Bearer fresh (stale token must never reach the API)", got)
		}
		w.WriteHeader(http.StatusOK)
		w.Write(usageResp(t, 40.0, "2026-07-26T07:00:00Z", 5.0, "2026-07-20T12:00:00Z"))
	}))
	defer srv.Close()

	tok := &updatingTokens{
		inner: &stubTokens{cred: credJSONExpiring(t, "stale", time.Now().Add(-time.Hour))},
		after: credJSONExpiring(t, "fresh", time.Now().Add(8*time.Hour)),
	}
	refresher := &swapRefresher{inner: &stubRefresher{}, tok: tok}
	c := &Collector{Tokens: tok, Refresher: refresher, API: newHTTPUsageAPI(t, srv.URL)}

	payload, ops, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect err = %v", err)
	}
	if got := atomic.LoadInt32(&apiCalls); got != 1 {
		t.Errorf("api calls = %d, want exactly 1 (no wasted 401 round-trip)", got)
	}
	if refresher.inner.calls != 1 {
		t.Errorf("refresher calls = %d, want 1", refresher.inner.calls)
	}
	if payload.SevenDay.Utilization != 40.0 {
		t.Errorf("payload = %+v", payload.SevenDay)
	}
	wantOps := []string{"refresh-token", "api-ok"}
	if len(ops) != 2 || ops[0] != wantOps[0] || ops[1] != wantOps[1] {
		t.Errorf("ops = %v, want %v", ops, wantOps)
	}
}

// TestCollector_StillExpiredAfterRefreshArmsGuard: if the refresh doesn't
// rotate the expired token (CLI idle / update no-op), the cycle fails without
// touching the API, and later cycles stay silent until the token changes.
func TestCollector_StillExpiredAfterRefreshArmsGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("api must not be called with an expired token")
	}))
	defer srv.Close()

	refresher := &stubRefresher{}
	c := &Collector{
		Tokens:    &stubTokens{cred: credJSONExpiring(t, "stale", time.Now().Add(-time.Hour))},
		Refresher: refresher,
		API:       newHTTPUsageAPI(t, srv.URL),
	}

	if _, _, err := c.Collect(context.Background()); err == nil {
		t.Fatal("Collect err = nil, want still-expired failure")
	}
	if refresher.calls != 1 {
		t.Errorf("refresher calls = %d, want 1", refresher.calls)
	}

	// Second cycle: token unchanged → skip without another refresh.
	if _, _, err := c.Collect(context.Background()); err == nil {
		t.Fatal("second Collect err = nil, want skip error")
	}
	if refresher.calls != 1 {
		t.Errorf("refresher calls = %d after skip cycle, want still 1", refresher.calls)
	}
}

// TestCollector_FreshExpiryNoRefresh: a token with a future expiresAt goes
// straight to the API without any refresh.
func TestCollector_FreshExpiryNoRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(usageResp(t, 10.0, "2026-07-26T07:00:00Z", 1.0, "2026-07-20T12:00:00Z"))
	}))
	defer srv.Close()

	refresher := &stubRefresher{}
	c := &Collector{
		Tokens:    &stubTokens{cred: credJSONExpiring(t, "tok", time.Now().Add(8*time.Hour))},
		Refresher: refresher,
		API:       newHTTPUsageAPI(t, srv.URL),
	}

	if _, _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect err = %v", err)
	}
	if refresher.calls != 0 {
		t.Errorf("refresher calls = %d, want 0 for a fresh token", refresher.calls)
	}
}

// TestCollector_RateLimitCarriesRetryAfter: a 429 with a Retry-After header
// must surface both the ErrRateLimit sentinel and the server-requested delay,
// so the daemon can back off exactly as instructed instead of guessing.
func TestCollector_RateLimitCarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := &Collector{
		Tokens:    &stubTokens{cred: credJSON(t, "tok")},
		Refresher: &stubRefresher{},
		API:       newHTTPUsageAPI(t, srv.URL),
	}

	_, _, err := c.Collect(context.Background())
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("err = %v, want ErrRateLimit", err)
	}
	if got := retryAfterFrom(err); got != 120*time.Second {
		t.Errorf("retryAfterFrom = %v, want 120s", got)
	}
}

// TestCollector_RateLimitWithoutHeader: a bare 429 still reports ErrRateLimit
// with a zero Retry-After, leaving the daemon to its own backoff.
func TestCollector_RateLimitWithoutHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := &Collector{
		Tokens:    &stubTokens{cred: credJSON(t, "tok")},
		Refresher: &stubRefresher{},
		API:       newHTTPUsageAPI(t, srv.URL),
	}

	_, _, err := c.Collect(context.Background())
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("err = %v, want ErrRateLimit", err)
	}
	if got := retryAfterFrom(err); got != 0 {
		t.Errorf("retryAfterFrom = %v, want 0 when header absent", got)
	}
}

// TestUsageAPI_SendsClaudeCodeUserAgent: requests must present themselves as
// claude-code/<version> — the default Go-http-client UA is an outlier the
// endpoint may throttle more aggressively.
func TestUsageAPI_SendsClaudeCodeUserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
		w.Write(usageResp(t, 1, "2026-07-26T07:00:00Z", 1, "2026-07-20T12:00:00Z"))
	}))
	defer srv.Close()

	api := newUsageAPI(srv.URL)
	if _, _, _, err := api.Fetch(context.Background(), "tok"); err != nil {
		t.Fatalf("Fetch err = %v", err)
	}
	if !strings.HasPrefix(gotUA, "claude-code/") {
		t.Errorf("User-Agent = %q, want claude-code/<version>", gotUA)
	}
}

// TestClaudeUserAgent_Fallback: version discovery must always yield a usable
// claude-code/<version> string, even when the CLI is not on PATH.
func TestClaudeUserAgent_Fallback(t *testing.T) {
	if got := claudeUserAgent(); !strings.HasPrefix(got, "claude-code/") {
		t.Errorf("claudeUserAgent() = %q, want claude-code/ prefix", got)
	}
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// updatingTokens returns inner's credentials first, then switches to `after`
// once Refresh has been called. This simulates the CLI rewriting the keychain.
type updatingTokens struct {
	inner   *stubTokens
	after   []byte
	swapped bool
}

func (u *updatingTokens) Credentials() ([]byte, error) {
	if u.swapped {
		return u.after, nil
	}
	return u.inner.Credentials()
}

// keyed so the collector's stubRefresher can trigger the swap; we use a tiny
// adapter because the collector owns the refresher.
type swapRefresher struct {
	inner *stubRefresher
	tok   *updatingTokens
}

func (s *swapRefresher) Refresh(ctx context.Context) error {
	err := s.inner.Refresh(ctx)
	s.tok.swapped = true
	return err
}
