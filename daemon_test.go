package main

import (
	"testing"
	"time"
)

// TestRateLimitDelay pins the 429 backoff policy: the server's Retry-After
// wins (clamped between the collect interval and the max backoff); without a
// header the wait doubles per consecutive rate-limit, capped at the max.
func TestRateLimitDelay(t *testing.T) {
	const interval = 5 * time.Minute
	cases := []struct {
		name       string
		retryAfter time.Duration
		prevWait   time.Duration
		want       time.Duration
	}{
		{"header shorter than interval clamps up", 120 * time.Second, interval, interval},
		{"header within range used as-is", 17 * time.Minute, interval, 17 * time.Minute},
		{"header above cap clamps down", 2 * time.Hour, interval, 30 * time.Minute},
		{"no header doubles previous wait", 0, interval, 10 * time.Minute},
		{"no header doubling caps at max", 0, 20 * time.Minute, 30 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rateLimitDelay(tc.retryAfter, tc.prevWait, interval); got != tc.want {
				t.Errorf("rateLimitDelay(%v, %v, %v) = %v, want %v",
					tc.retryAfter, tc.prevWait, interval, got, tc.want)
			}
		})
	}
}
