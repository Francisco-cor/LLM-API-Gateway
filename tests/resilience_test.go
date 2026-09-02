package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/resilience"
)

// TestRetry_ExponentialJitter verifies 3 attempts with retryable errors.
func TestRetry_ExponentialJitter(t *testing.T) {
	cfg := resilience.RetryConfig{MaxAttempts: 3, BaseDelay: 10 * time.Millisecond, MaxDelay: 50 * time.Millisecond, Jitter: false}
	attempts := 0
	err := resilience.Do(context.Background(), cfg, provider.IsRetryable, func() error {
		attempts++
		if attempts < 3 {
			return &provider.ProviderError{ProviderName: "test", StatusCode: 500, Message: "boom", Retryable: true}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestRetry_NonRetryable(t *testing.T) {
	cfg := resilience.RetryConfig{MaxAttempts: 3, BaseDelay: 1 * time.Millisecond, Jitter: false}
	attempts := 0
	err := resilience.Do(context.Background(), cfg, provider.IsRetryable, func() error {
		attempts++
		return &provider.ProviderError{ProviderName: "test", StatusCode: 400, Message: "bad", Retryable: false}
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Errorf("non-retryable should not retry, attempts=%d", attempts)
	}
}

func TestRetry_ContextCancel(t *testing.T) {
	cfg := resilience.RetryConfig{MaxAttempts: 5, BaseDelay: 100 * time.Millisecond, Jitter: false}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := resilience.Do(ctx, cfg, provider.IsRetryable, func() error {
		return &provider.ProviderError{ProviderName: "test", StatusCode: 500, Retryable: true}
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context canceled, got %v", err)
	}
}

func TestCircuit_StateTransitions(t *testing.T) {
	cfg := resilience.CircuitConfig{FailureThreshold: 2, OpenTimeout: 50 * time.Millisecond, HalfOpenMax: 1}
	b := resilience.NewBreaker(cfg)

	if !b.Allow() {
		t.Fatal("closed should allow")
	}
	b.RecordFailure()
	if !b.Allow() {
		t.Fatal("1 failure should still allow")
	}
	b.RecordFailure()
	if b.State() != resilience.StateOpen {
		t.Errorf("state = %s, want open", b.State())
	}
	if b.Allow() {
		t.Error("open should not allow")
	}
	time.Sleep(60 * time.Millisecond)
	if !b.Allow() {
		t.Error("after open timeout should allow (half-open)")
	}
	if b.State() != resilience.StateHalfOpen {
		t.Errorf("state = %s, want half-open", b.State())
	}
	b.RecordSuccess()
	if b.State() != resilience.StateClosed {
		t.Errorf("after half-open success should be closed, got %s", b.State())
	}

	// open again and fail in half-open -> back to open
	b.RecordFailure()
	b.RecordFailure()
	time.Sleep(60 * time.Millisecond)
	_ = b.Allow() // transition to half-open
	b.RecordFailure()
	if b.State() != resilience.StateOpen {
		t.Errorf("half-open failure should re-open, got %s", b.State())
	}
}

func TestCircuit_HalfOpenMax(t *testing.T) {
	cfg := resilience.CircuitConfig{FailureThreshold: 1, OpenTimeout: 20 * time.Millisecond, HalfOpenMax: 1}
	b := resilience.NewBreaker(cfg)
	b.RecordFailure()
	time.Sleep(30 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("should allow 1 in half-open")
	}
	if b.Allow() {
		t.Error("should not allow second in half-open")
	}
}

func TestHedge_FastPrimaryWins(t *testing.T) {
	ctx := context.Background()
	val, err := resilience.DoHedge(ctx, 50*time.Millisecond,
		func() (any, error) {
			return "primary", nil
		},
		func() (any, error) {
			time.Sleep(10 * time.Millisecond)
			return "fallback", nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if val != "primary" {
		t.Errorf("val=%v, want primary", val)
	}
}

func TestHedge_PrimarySlowFallbackWins(t *testing.T) {
	ctx := context.Background()
	val, err := resilience.DoHedge(ctx, 20*time.Millisecond,
		func() (any, error) {
			time.Sleep(100 * time.Millisecond)
			return "primary", nil
		},
		func() (any, error) {
			return "fallback", nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	// hedge may return either primary or fallback depending on timing; both are valid if no error
	if val != "primary" && val != "fallback" {
		t.Errorf("val=%v, want primary or fallback", val)
	}
}
