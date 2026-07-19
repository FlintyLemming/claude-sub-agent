package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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
