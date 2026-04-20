package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"time"
)

// RetryConfig controls WithRetry backoff. Zero value is usable — InitialDelay
// defaults to 200ms, MaxDelay to 10s, MaxAttempts to 3, BackoffFactor to 2.
type RetryConfig struct {
	MaxAttempts   int
	InitialDelay  time.Duration
	MaxDelay      time.Duration
	BackoffFactor float64
	Jitter        bool
}

func defaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:   3,
		InitialDelay:  200 * time.Millisecond,
		MaxDelay:      10 * time.Second,
		BackoffFactor: 2.0,
		Jitter:        true,
	}
}

// WithRetry runs fn up to cfg.MaxAttempts times with exponential backoff.
// Context cancellation and non-retryable errors exit immediately. net.Error
// temporary/timeout failures are retryable; context errors are not.
//
// The pattern is adapted from the go-development skill's references/resilience.md
// and is used for transient HTTP in fetch.go. Do NOT use it for `claude -p`
// rate limits: scribe's design reacts to ErrRateLimit by bailing so the next
// cron invocation resumes cleanly — an in-process retry would hold claude
// quota hostage past the cron window.
func WithRetry(ctx context.Context, cfg RetryConfig, fn func() error) error {
	if cfg.MaxAttempts == 0 {
		cfg = defaultRetryConfig()
	}
	delay := cfg.InitialDelay
	if delay == 0 {
		delay = 200 * time.Millisecond
	}
	maxDelay := cfg.MaxDelay
	if maxDelay == 0 {
		maxDelay = 10 * time.Second
	}
	factor := cfg.BackoffFactor
	if factor == 0 {
		factor = 2.0
	}

	var last error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn()
		if err == nil {
			return nil
		}
		last = err
		if !isRetryable(err) {
			return err
		}
		if attempt == cfg.MaxAttempts {
			break
		}
		d := delay
		if cfg.Jitter {
			d = addJitter(d)
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
		delay = min(time.Duration(float64(delay)*factor), maxDelay)
	}
	return fmt.Errorf("after %d attempts: %w", cfg.MaxAttempts, last)
}

func isRetryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if netErr, ok := errors.AsType[net.Error](err); ok {
		return netErr.Timeout()
	}
	// Conservative default — unknown errors aren't retried so we don't mask
	// bugs as transient failures.
	return false
}

func addJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d + time.Duration(rand.Int64N(int64(d/2)))
}
