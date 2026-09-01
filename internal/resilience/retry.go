package resilience

import (
	"context"
	"math/rand"
	"time"
)

// RetryConfig controls retry behavior per provider attempt.
// BaseDelay * 2^attempt +/- jitter
type RetryConfig struct {
	MaxAttempts int           // total attempts (1 = no retry)
	BaseDelay   time.Duration // e.g. 200ms
	MaxDelay    time.Duration // cap, e.g. 2s
	Jitter      bool
}

func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   200 * time.Millisecond,
		MaxDelay:    2 * time.Second,
		Jitter:      true,
	}
}

// Do retries fn up to cfg.MaxAttempts times if isRetryable returns true.
// Sleeps with exponential backoff + jitter between attempts.
// Respects ctx cancellation.
func Do(ctx context.Context, cfg RetryConfig, isRetryable func(error) bool, fn func() error) error {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 1
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 200 * time.Millisecond
	}
	var lastErr error
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
			if !isRetryable(err) {
				return err
			}
			if attempt == cfg.MaxAttempts-1 {
				return err
			}
			delay := cfg.BaseDelay * time.Duration(1<<attempt)
			if delay > cfg.MaxDelay && cfg.MaxDelay > 0 {
				delay = cfg.MaxDelay
			}
			if cfg.Jitter {
				// +-25% jitter
				j := time.Duration(rand.Int63n(int64(delay)/2)) - delay/4
				delay += j
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return lastErr
}
