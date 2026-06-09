package tests

import (
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/ratelimit"
)

func TestLimiter_AllowsBurstThenBlocks(t *testing.T) {
	limiter := ratelimit.New(60, 2) // 1 token/sec, burst of 2

	if !limiter.Allow("key-a") {
		t.Fatal("first request should be allowed (burst)")
	}
	if !limiter.Allow("key-a") {
		t.Fatal("second request should be allowed (burst)")
	}
	if limiter.Allow("key-a") {
		t.Fatal("third immediate request should be rejected")
	}
}

func TestLimiter_KeysAreIndependent(t *testing.T) {
	limiter := ratelimit.New(60, 1)

	if !limiter.Allow("key-a") {
		t.Fatal("key-a first request should be allowed")
	}
	if !limiter.Allow("key-b") {
		t.Fatal("key-b should have its own bucket")
	}
	if limiter.Allow("key-a") {
		t.Fatal("key-a should be exhausted")
	}
}

func TestLimiter_RetryAfterIsZeroWhenAllowed(t *testing.T) {
	limiter := ratelimit.New(60, 1)

	if got := limiter.RetryAfter("key-a"); got != 0 {
		t.Errorf("RetryAfter on fresh bucket = %v, want 0", got)
	}
}

func TestLimiter_RetryAfterIsPositiveWhenExhausted(t *testing.T) {
	limiter := ratelimit.New(60, 1) // 1 token/sec, burst of 1

	limiter.Allow("key-a") // consumes the only token

	retryAfter := limiter.RetryAfter("key-a")
	if retryAfter <= 0 || retryAfter > time.Second {
		t.Errorf("RetryAfter = %v, want between 0 and 1s", retryAfter)
	}
}
