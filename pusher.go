package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
)

// maxPushRetries is the number of times a failed push is retried after the
// initial attempt. The design doc specifies 3 retries with 1s/2s/4s backoff.
const maxPushRetries = 3

// defaultBaseBackoff is the base delay before the first retry. The design doc
// specifies 1s (then 2s, 4s for retries 2 and 3).
const defaultBaseBackoff = 1 * time.Second

// Pusher POSTs a Payload to the configured push URL, retrying transient
// failures with exponential backoff.
type Pusher struct {
	URL         string
	Client      *http.Client
	baseBackoff time.Duration
}

// NewPusher builds a Pusher with a sane default timeout and backoff.
func NewPusher(url string) *Pusher {
	return &Pusher{
		URL:         url,
		Client:      &http.Client{Timeout: 30 * time.Second},
		baseBackoff: defaultBaseBackoff,
	}
}

// Push marshals and POSTs the payload. It retries on 5xx and transport errors
// up to maxPushRetries times (1s/2s/4s backoff). A non-retryable HTTP status
// (e.g. 4xx) returns immediately. Returns nil once the server accepts (2xx).
func (p *Pusher) Push(ctx context.Context, payload *Payload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	var lastErr error
	backoff := p.baseBackoff
	for attempt := 0; attempt <= maxPushRetries; attempt++ {
		if attempt > 0 {
			log.Printf("pusher: retrying after %v (attempt %d/%d)", backoff, attempt, maxPushRetries)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		err := p.doPost(ctx, body)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable(err) {
			return err
		}
	}
	return fmt.Errorf("push failed after %d attempts: %w", maxPushRetries+1, lastErr)
}

// doPost performs a single POST. Returns nil on 2xx.
func (p *Pusher) doPost(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.Client.Do(req)
	if err != nil {
		return &pushError{kind: kindTransport, err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	kind := kindStatusClient
	if resp.StatusCode >= 500 {
		kind = kindStatusServer
	}
	return &pushError{kind: kind, status: resp.StatusCode}
}

// retryable reports whether a push error warrants another attempt.
func retryable(err error) bool {
	var pe *pushError
	if errors.As(err, &pe) {
		return pe.kind == kindTransport || pe.kind == kindStatusServer
	}
	return false
}

const (
	kindTransport    = iota // network/timeout/DNS — usually transient
	kindStatusServer        // 5xx — usually transient
	kindStatusClient        // 4xx — not worth retrying
)

type pushError struct {
	kind   int
	status int
	err    error
}

func (e *pushError) Error() string {
	switch e.kind {
	case kindTransport:
		return fmt.Sprintf("transport: %v", e.err)
	case kindStatusServer, kindStatusClient:
		return fmt.Sprintf("http %d", e.status)
	}
	return "push error"
}

func (e *pushError) Unwrap() error { return e.err }
