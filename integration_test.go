package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestCollectAndPush_Pipeline is an integration test that wires the real
// Collector and Pusher types together (only the external command surface is
// stubbed) and asserts the end-to-end pipeline produces a spec-conformant POST.
func TestCollectAndPush_Pipeline(t *testing.T) {
	// --- Usage API mock: returns the sample payload from the design doc. ---
	usageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-beta") != anthropicBetaHeader {
			t.Errorf("usage api: missing beta header")
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{
		  "seven_day": {"utilization": 45.2, "resets_at": "2026-07-08T12:00:00Z"},
		  "five_hour": {"utilization": 12.8, "resets_at": "2026-07-01T15:00:00Z"},
		  "extra_usage": {"is_enabled": true, "used_credits": 5, "monthly_limit": 100}
		}`))
	}))
	defer usageSrv.Close()

	// --- Push target mock: capture exactly one POST. ---
	var received atomic.Value // []byte
	pushSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("push: Content-Type = %q", ct)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer v2-secret" {
			t.Errorf("push: Authorization = %q, want Bearer v2-secret", auth)
		}
		buf := new(bytes.Buffer)
		buf.ReadFrom(r.Body)
		received.Store(buf.Bytes())
		w.WriteHeader(http.StatusOK)
	}))
	defer pushSrv.Close()

	// Fast push retries so a flaky first attempt won't slow the test.
	collector := &Collector{
		Tokens:    &stubTokens{cred: credJSON(t, "integration-token")},
		Refresher: &stubRefresher{},
		API:       newUsageAPI(usageSrv.URL),
	}
	pusher := testPusher(pushSrv.URL, 5*time.Second, time.Millisecond)
	pusher.Token = "v2-secret"

	// Drive the same pipeline the daemon uses.
	ctx := context.Background()
	collectOnce(ctx, daemonConfig{
		collector: collector,
		pusher:    pusher,
	})

	raw, ok := received.Load().([]byte)
	if !ok {
		t.Fatal("push server received no request")
	}

	// The POST body must match the v2 ClaudePushRequest schema exactly:
	// seven_day + five_hour only (both required), no extra_usage.
	var got Payload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal posted body: %v\nbody: %s", err, raw)
	}
	if got.SevenDay.Utilization != 45.2 || got.SevenDay.ResetsAt != "2026-07-08T12:00:00Z" {
		t.Errorf("seven_day = %+v", got.SevenDay)
	}
	if got.FiveHour.Utilization != 12.8 || got.FiveHour.ResetsAt != "2026-07-01T15:00:00Z" {
		t.Errorf("five_hour = %+v", got.FiveHour)
	}

	// Strict check: no extra_usage key may be present in the raw JSON.
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawMap); err != nil {
		t.Fatal(err)
	}
	if _, present := rawMap["extra_usage"]; present {
		t.Errorf("payload must not include extra_usage, body: %s", raw)
	}
}
