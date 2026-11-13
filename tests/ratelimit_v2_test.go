package tests

import (
	"sync"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/config"
	"github.com/fcordero/llm-api-gateway/internal/ratelimit"
)

// TestLimiter_TokenBucketIsNotRequestBurst guards the configuration bug where
// a prompt larger than the request burst was rejected forever: request bursts
// are counted in requests, token budgets in tokens.
func TestLimiter_TokenBucketIsNotRequestBurst(t *testing.T) {
	limiter := ratelimit.New(60, 10) // request burst = 10 requests
	if limiter.AllowWithLimits("client:tokens", 250, 60, 10) {
		t.Fatal("250 tokens must not fit in a 10-token bucket")
	}
	if !limiter.AllowWithLimits("client:tokens", 250, 60000, 10000) {
		t.Fatal("250 tokens must fit in the configured 10000-token bucket")
	}
}

func TestLimiter_AllowN_TokenAware(t *testing.T) {
	limiter := ratelimit.New(60, 10) // 60 rpm, burst 10

	if !limiter.AllowN("key-a", 5) {
		t.Fatal("should allow 5 tokens from burst 10")
	}
	if limiter.AllowN("key-a", 6) {
		t.Fatal("should block when only 5 left but need 6")
	}
	if !limiter.AllowN("key-a", 5) {
		t.Fatal("should allow remaining 5")
	}
}

func TestLimiter_CloseIsIdempotent(t *testing.T) {
	limiter := ratelimit.New(60, 1)
	if err := limiter.Close(); err != nil {
		t.Fatalf("first Close returned error: %v", err)
	}
	if err := limiter.Close(); err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}
}

func TestLimiter_PerKeyOverride(t *testing.T) {
	limiter := ratelimit.New(60, 1)
	if !limiter.AllowWithLimits("override-key", 5, 600, 5) {
		t.Fatal("override should allow its configured burst")
	}
	if limiter.AllowWithLimits("override-key", 1, 600, 5) {
		t.Fatal("override burst should be exhausted")
	}
}

func TestLimiter_TTLExpiration(t *testing.T) {
	limiter := ratelimit.New(600, 1) // high rate so refill fast, but test TTL path via Tokens
	key := "ttl-key"
	if !limiter.Allow(key) {
		t.Fatal("first allow")
	}
	if limiter.Allow(key) {
		t.Fatal("second immediate should block (burst 1, rate 10/s)")
	}
	// tokens should be <1
	if limiter.Tokens(key) >= 1 {
		t.Errorf("tokens should be <1 after burst exhausted, got %f", limiter.Tokens(key))
	}
}

func TestLimiter_ShardedConcurrency(t *testing.T) {
	limiter := ratelimit.New(6000, 100)
	var wg sync.WaitGroup
	success := 0
	var mu sync.Mutex
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if limiter.Allow("concurrent-key") {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if success != 100 {
		t.Errorf("concurrent allows = %d, want 100 (burst limit)", success)
	}
}

func TestLimiter_Bench(t *testing.T) {
	limiter := ratelimit.New(60000, 1000)
	start := time.Now()
	for i := 0; i < 10000; i++ {
		limiter.Allow("bench-key")
	}
	elapsed := time.Since(start)
	// rough perf check: 10k allows should be <100ms with sharded
	if elapsed > 200*time.Millisecond {
		t.Errorf("10k allows took %v, want <200ms", elapsed)
	}
}

func TestOverride_Resolve(t *testing.T) {
	cfg := config.RateLimitConfig{
		RequestsPerMinute: 60,
		Burst:             10,
		Overrides: []config.RateLimitOverride{
			{Tenant: "pro", ModelPattern: "gpt-4*", RPM: 600, Burst: 60},
			{Tenant: "*", ModelPattern: "*", RPM: 100, Burst: 20},
		},
	}
	store := ratelimit.NewOverrideStore(cfg)
	// Tenant-specific beats a global wildcard regardless of YAML order.
	rpm, burst, matched := store.ResolveScoped("pro", "gpt-4o")
	if !matched || rpm != 600 || burst != 60 {
		t.Fatalf("pro/gpt-4o rpm=%d burst=%d matched=%v, want 600/60 true", rpm, burst, matched)
	}
	// Other tenants fall back to the wildcard rule.
	rpm, burst, matched = store.ResolveScoped("free", "gpt-4o")
	if !matched || rpm != 100 || burst != 20 {
		t.Fatalf("free/gpt-4o rpm=%d burst=%d matched=%v, want 100/20 true", rpm, burst, matched)
	}
	// A rule that only sets RPM keeps the default burst instead of inheriting
	// a burst from another rule.
	cfg2 := config.RateLimitConfig{
		RequestsPerMinute: 60,
		Burst:             10,
		Overrides: []config.RateLimitOverride{
			{Tenant: "enterprise", ModelPattern: "claude-*", RPM: 1000},
		},
	}
	store2 := ratelimit.NewOverrideStore(cfg2)
	rpm2, burst2, _ := store2.ResolveScoped("enterprise", "claude-sonnet-4-6")
	if rpm2 != 1000 || burst2 != 10 {
		t.Errorf("resolve enterprise/claude rpm=%d burst=%d, want 1000/10", rpm2, burst2)
	}
	rpm3, burst3, matched3 := store2.ResolveScoped("free", "claude-sonnet-4-6")
	if matched3 || rpm3 != 60 || burst3 != 10 {
		t.Errorf("resolve free/claude rpm=%d burst=%d matched=%v, want default 60/10 false", rpm3, burst3, matched3)
	}
}

func TestEstimateTokens(t *testing.T) {
	if ratelimit.EstimateTokens(0) != 1 {
		t.Error("0 chars should be 1 token")
	}
	if ratelimit.EstimateTokens(400) != 100 {
		t.Errorf("400 chars = %d tokens, want 100", ratelimit.EstimateTokens(400))
	}
}
