package resilience

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/provider"
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
// Sleeps with exponential backoff + jitter between attempts, but never less
// than the provider's Retry-After hint. Respects ctx cancellation before and
// during each attempt.
func Do(ctx context.Context, cfg RetryConfig, isRetryable func(error) bool, fn func() error) error {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 1
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 200 * time.Millisecond
	}
	var lastErr error
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
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
			delay := backoff(cfg, attempt)
			if retryAfter := retryAfterOf(err); retryAfter > delay {
				delay = retryAfter
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

// backoff computes BaseDelay*2^attempt with a hard exponent cap so a very
// large MaxAttempts cannot overflow time.Duration into a negative delay.
func backoff(cfg RetryConfig, attempt int) time.Duration {
	shift := attempt
	if shift > 20 {
		shift = 20
	}
	delay := cfg.BaseDelay * time.Duration(1<<shift)
	if cfg.MaxDelay > 0 && delay > cfg.MaxDelay {
		delay = cfg.MaxDelay
	}
	if cfg.Jitter {
		// +-25% jitter
		jitterRange := int64(delay) / 2
		if jitterRange > 0 {
			j := time.Duration(rand.Int63n(jitterRange)) - delay/4
			delay += j
		}
	}
	if delay < 0 {
		delay = 0
	}
	return delay
}

// retryAfterOf extracts the provider's Retry-After hint from a wrapped error.
func retryAfterOf(err error) time.Duration {
	var pe *provider.ProviderError
	if errors.As(err, &pe) && pe.RetryAfter > 0 {
		return pe.RetryAfter
	}
	return 0
}
