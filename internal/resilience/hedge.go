package resilience

import (
	"context"
	"time"
)

// Hedge executes primary and, after delay, fallback in parallel, returning first success.
// If primary succeeds before hedge fires, fallback is never started.
// If hedge is not enabled, just runs primary.
type HedgeConfig struct {
	Enabled bool
	Delay   time.Duration
}

// DoHedge runs primary; if it hasn't succeeded within delay, runs fallback concurrently.
func DoHedge(ctx context.Context, delay time.Duration, primary func() (any, error), fallback func() (any, error)) (any, error) {
	if delay <= 0 {
		delay = 300 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		val any
		err error
	}

	primaryCh := make(chan result, 1)
	go func() {
		v, e := primary()
		select {
		case primaryCh <- result{v, e}:
		case <-ctx.Done():
		}
	}()

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case r := <-primaryCh:
		return r.val, r.err
	case <-timer.C:
		// hedge
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	fallbackCh := make(chan result, 1)
	go func() {
		v, e := fallback()
		select {
		case fallbackCh <- result{v, e}:
		case <-ctx.Done():
		}
	}()

	select {
	case r := <-primaryCh:
		if r.err == nil {
			return r.val, nil
		}
		// primary failed, wait for fallback
		select {
		case fr := <-fallbackCh:
			return fr.val, fr.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case r := <-fallbackCh:
		return r.val, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
