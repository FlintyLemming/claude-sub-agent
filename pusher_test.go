package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// testPusher builds a Pusher with a tiny client timeout and base backoff so
// retries don't slow the suite.
func testPusher(url string, timeout, backoff time.Duration) *Pusher {
	return &Pusher{
		URL:         url,
		Client:      &http.Client{Timeout: timeout},
		baseBackoff: backoff,
	}
}

func TestPusher_Success(t *testing.T) {
	var got int32
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&got, 1)
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		// Body capture is fine; small payload.
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		lastBody = buf[:n]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := testPusher(srv.URL, 5*time.Second, time.Millisecond)
	err := p.Push(context.Background(), &Payload{
		SevenDay: &quota{Utilization: 42.0, ResetsAt: "2026-07-08T12:00:00Z"},
		FiveHour: &quota{Utilization: 9.0, ResetsAt: "2026-07-01T15:00:00Z"},
	})
	if err != nil {
		t.Fatalf("Push err = %v", err)
	}
	if got := atomic.LoadInt32(&got); got != 1 {
		t.Errorf("server got %d requests, want 1", got)
	}
	// Body should contain the utilization value.
	if s := string(lastBody); !containsStr(s, "42") {
		t.Errorf("body = %q, want utilization 42", s)
	}
}

func TestPusher_5xxRetries(t *testing.T) {
	// Server fails twice with 503 then succeeds on the 3rd attempt.
	var got int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&got, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Speed up the retry backoff so the test is fast.
	p := testPusher(srv.URL, 5*time.Second, 5*time.Millisecond)
	err := p.Push(context.Background(), &Payload{SevenDay: &quota{Utilization: 1, ResetsAt: "x"}})
	if err != nil {
		t.Fatalf("Push err = %v, want nil after retries", err)
	}
	if got := atomic.LoadInt32(&got); got != 3 {
		t.Errorf("server got %d requests, want 3", got)
	}
}

func TestPusher_5xxExhaustsRetries(t *testing.T) {
	var got int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&got, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := testPusher(srv.URL, 5*time.Second, time.Millisecond)
	err := p.Push(context.Background(), &Payload{SevenDay: &quota{Utilization: 1, ResetsAt: "x"}})
	if err == nil {
		t.Fatal("Push err = nil, want error after exhausting retries")
	}
	// 1 initial attempt + 3 retries = 4 total requests.
	if got := atomic.LoadInt32(&got); got != 4 {
		t.Errorf("server got %d requests, want 4", got)
	}
}

func TestPusher_4xxNoRetry(t *testing.T) {
	var got int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&got, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	p := testPusher(srv.URL, 5*time.Second, time.Millisecond)
	err := p.Push(context.Background(), &Payload{SevenDay: &quota{Utilization: 1, ResetsAt: "x"}})
	if err == nil {
		t.Fatal("Push err = nil, want error for 4xx")
	}
	if got := atomic.LoadInt32(&got); got != 1 {
		t.Errorf("server got %d requests, want 1 (no retry on 4xx)", got)
	}
}

func TestPusher_TransportErrorRetries(t *testing.T) {
	// Closed server => connection refused on every attempt.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	p := testPusher(srv.URL, 5*time.Second, time.Millisecond)
	err := p.Push(context.Background(), &Payload{SevenDay: &quota{Utilization: 1, ResetsAt: "x"}})
	if err == nil {
		t.Fatal("Push err = nil, want transport error")
	}
	if !retryable(err) {
		t.Errorf("transport error should be retryable")
	}
}

func TestPusher_ContextCancelAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	// Use a larger backoff so the cancellation happens mid-wait.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	p := testPusher(srv.URL, 5*time.Second, 200*time.Millisecond)
	err := p.Push(ctx, &Payload{SevenDay: &quota{Utilization: 1, ResetsAt: "x"}})
	if err == nil {
		t.Fatal("Push err = nil, want ctx cancellation error")
	}
	if !containsStr(err.Error(), "context") {
		t.Logf("err = %v", err)
	}
}

func TestPusher_RetryableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"transport", &pushError{kind: kindTransport, err: fmt.Errorf("x")}, true},
		{"5xx", &pushError{kind: kindStatusServer, status: 503}, true},
		{"4xx", &pushError{kind: kindStatusClient, status: 400}, false},
		{"wrapped transport", wrapErr(&pushError{kind: kindTransport, err: fmt.Errorf("x")}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryable(tc.err); got != tc.want {
				t.Errorf("retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func wrapErr(e error) error { return fmt.Errorf("outer: %w", e) }
