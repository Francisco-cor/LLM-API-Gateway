package resilience

import (
	"context"
	"time"
)

// HedgeConfig controls hedged requests.
type HedgeConfig struct {
	Enabled bool
	Delay   time.Duration
}

// DoHedge runs primary; if it hasn't succeeded within delay, runs fallback
// concurrently. It returns the first success. Callers that need per-branch
// cancellation (so the loser stops doing billable work) should use
// DoHedgeContext.
func DoHedge(ctx context.Context, delay time.Duration, primary func() (any, error), fallback func() (any, error)) (any, error) {
	return DoHedgeContext(ctx, delay,
		func(context.Context) (any, error) { return primary() },
		func(context.Context) (any, error) { return fallback() },
	)
}

// DoHedgeContext is DoHedge with a dedicated context per branch. The winner
// cancels the loser's context as soon as it is known, so a hedged provider call
// is not left running and billing after the client already has an answer.
//
// A branch that fails because its own context was cancelled by the winner is
// not an error of its own: the winner's result is returned.
func DoHedgeContext(ctx context.Context, delay time.Duration, primary, fallback func(context.Context) (any, error)) (any, error) {
	if delay <= 0 {
		delay = 300 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	primaryCtx, cancelPrimary := context.WithCancel(ctx)
	defer cancelPrimary()
	fallbackCtx, cancelFallback := context.WithCancel(ctx)
	defer cancelFallback()

	type result struct {
		val any
		err error
	}

	settle := func(r result, cancelLoser context.CancelFunc) (any, error) {
		if r.err == nil {
			cancelLoser()
		}
		return r.val, r.err
	}

	primaryCh := make(chan result, 1)
	go func() {
		v, e := primary(primaryCtx)
		select {
		case primaryCh <- result{v, e}:
		case <-ctx.Done():
		}
	}()

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case r := <-primaryCh:
		return settle(r, cancelFallback)
	case <-timer.C:
		// hedge
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	fallbackCh := make(chan result, 1)
	go func() {
		v, e := fallback(fallbackCtx)
		select {
		case fallbackCh <- result{v, e}:
		case <-ctx.Done():
		}
	}()

	select {
	case r := <-primaryCh:
		if r.err == nil {
			cancelFallback()
			return r.val, nil
		}
		// primary failed, wait for fallback
		select {
		case fr := <-fallbackCh:
			if fr.err == nil {
				cancelPrimary()
			}
			return fr.val, fr.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case r := <-fallbackCh:
		if r.err == nil {
			cancelPrimary()
			return r.val, nil
		}
		// A hedge failure must not mask a still-running primary that may
		// succeed. Wait for it before returning an error.
		select {
		case pr := <-primaryCh:
			return pr.val, pr.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
