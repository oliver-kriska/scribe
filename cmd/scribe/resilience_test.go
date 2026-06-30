package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeNetError implements net.Error with a configurable Timeout answer.
type fakeNetError struct{ timeout bool }

func (e *fakeNetError) Error() string   { return "fake net error" }
func (e *fakeNetError) Timeout() bool   { return e.timeout }
func (e *fakeNetError) Temporary() bool { return e.timeout }

// fastRetry keeps test wall time negligible.
func fastRetry(attempts int) RetryConfig {
	return RetryConfig{
		MaxAttempts:   attempts,
		InitialDelay:  time.Millisecond,
		MaxDelay:      5 * time.Millisecond,
		BackoffFactor: 2.0,
	}
}

func TestDefaultRetryConfig(t *testing.T) {
	cfg := defaultRetryConfig()
	if cfg.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d", cfg.MaxAttempts)
	}
	if cfg.InitialDelay != 200*time.Millisecond || cfg.MaxDelay != 10*time.Second {
		t.Errorf("delays = %v / %v", cfg.InitialDelay, cfg.MaxDelay)
	}
	if cfg.BackoffFactor != 2.0 || !cfg.Jitter {
		t.Errorf("factor/jitter = %v / %v", cfg.BackoffFactor, cfg.Jitter)
	}
}

func TestWithRetry(t *testing.T) {
	ctx := context.Background()

	t.Run("first-try success calls fn once", func(t *testing.T) {
		calls := 0
		err := WithRetry(ctx, fastRetry(3), func() error {
			calls++
			return nil
		})
		if err != nil || calls != 1 {
			t.Errorf("err=%v calls=%d", err, calls)
		}
	})

	t.Run("non-retryable error returns immediately and unwrapped", func(t *testing.T) {
		boom := errors.New("boom")
		calls := 0
		err := WithRetry(ctx, fastRetry(3), func() error {
			calls++
			return boom
		})
		if !errors.Is(err, boom) || calls != 1 {
			t.Errorf("err=%v calls=%d, want unwrapped boom after 1 call", err, calls)
		}
	})

	t.Run("retryable error retries to exhaustion and wraps", func(t *testing.T) {
		calls := 0
		timeoutErr := &fakeNetError{timeout: true}
		err := WithRetry(ctx, fastRetry(3), func() error {
			calls++
			return timeoutErr
		})
		if calls != 3 {
			t.Errorf("calls = %d, want 3", calls)
		}
		if err == nil || !errors.Is(err, timeoutErr) {
			t.Errorf("exhaustion error should wrap the last error: %v", err)
		}
		if got := err.Error(); !strings.Contains(got, "after 3 attempts") {
			t.Errorf("error = %q, want attempt count", got)
		}
	})

	t.Run("recovers when a later attempt succeeds", func(t *testing.T) {
		calls := 0
		err := WithRetry(ctx, fastRetry(3), func() error {
			calls++
			if calls < 3 {
				return &fakeNetError{timeout: true}
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Errorf("err=%v calls=%d", err, calls)
		}
	})

	t.Run("canceled context stops before calling fn", func(t *testing.T) {
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		err := WithRetry(canceled, fastRetry(3), func() error {
			calls++
			return nil
		})
		if !errors.Is(err, context.Canceled) || calls != 0 {
			t.Errorf("err=%v calls=%d", err, calls)
		}
	})

	t.Run("zero config picks up defaults", func(t *testing.T) {
		// MaxAttempts == 0 must not mean "never run fn".
		calls := 0
		err := WithRetry(ctx, RetryConfig{}, func() error {
			calls++
			return errors.New("non-retryable")
		})
		if err == nil || calls != 1 {
			t.Errorf("err=%v calls=%d", err, calls)
		}
	})
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"context canceled", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"net timeout", &fakeNetError{timeout: true}, true},
		{"net non-timeout", &fakeNetError{timeout: false}, false},
		{"plain error", errors.New("x"), false},
		{"wrapped net timeout", errors.Join(errors.New("ctx"), &fakeNetError{timeout: true}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryable(tt.err); got != tt.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestAddJitter(t *testing.T) {
	if got := addJitter(0); got != 0 {
		t.Errorf("addJitter(0) = %v", got)
	}
	if got := addJitter(-time.Second); got != -time.Second {
		t.Errorf("addJitter(-1s) = %v", got)
	}
	base := 100 * time.Millisecond
	for range 50 {
		got := addJitter(base)
		if got < base || got >= base+base/2 {
			t.Fatalf("addJitter(%v) = %v, want [d, d+d/2)", base, got)
		}
	}
}
