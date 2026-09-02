package budget

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Manager tracks monthly token/USD budgets per tenant.
// Uses Redis if available, else in-memory (resets on restart).
type Manager struct {
	mu      sync.Mutex
	mem     map[string]*usage
	redis   *redis.Client
	enabled bool
	tokens  int
	usd     float64
}

type usage struct {
	tokens int
	usd    float64
	month  string // "2026-09"
}

func New(tokens int, usd float64, redisClient *redis.Client) *Manager {
	return &Manager{
		mem:     make(map[string]*usage),
		redis:   redisClient,
		enabled: tokens > 0 || usd > 0,
		tokens:  tokens,
		usd:     usd,
	}
}

func monthKey() string {
	return time.Now().Format("2006-01")
}

// Check returns error if tenant exceeds monthly budget (429 insufficient_quota).
func (m *Manager) Check(tenant string) error {
	if !m.enabled || tenant == "" {
		return nil
	}
	if m.redis != nil {
		return m.checkRedis(tenant)
	}
	return m.checkMem(tenant)
}

func (m *Manager) checkMem(tenant string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.mem[tenant]
	if u == nil || u.month != monthKey() {
		return nil
	}
	if m.tokens > 0 && u.tokens >= m.tokens {
		return fmt.Errorf("insufficient_quota: monthly token budget exceeded (%d/%d)", u.tokens, m.tokens)
	}
	if m.usd > 0 && u.usd >= m.usd {
		return fmt.Errorf("insufficient_quota: monthly USD budget exceeded (%.2f/%.2f)", u.usd, m.usd)
	}
	return nil
}

func (m *Manager) checkRedis(tenant string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	key := fmt.Sprintf("budget:%s:%s", tenant, monthKey())
	val, err := m.redis.HMGet(ctx, key, "tokens", "usd").Result()
	if err != nil {
		return nil // fail-open
	}
	var tokens int
	var usd float64
	if len(val) >= 1 && val[0] != nil {
		fmt.Sscanf(fmt.Sprintf("%v", val[0]), "%d", &tokens)
	}
	if len(val) >= 2 && val[1] != nil {
		fmt.Sscanf(fmt.Sprintf("%v", val[1]), "%f", &usd)
	}
	if m.tokens > 0 && tokens >= m.tokens {
		return fmt.Errorf("insufficient_quota: monthly token budget exceeded")
	}
	if m.usd > 0 && usd >= m.usd {
		return fmt.Errorf("insufficient_quota: monthly USD budget exceeded")
	}
	return nil
}

// Record adds tokens/usd to tenant's monthly usage.
func (m *Manager) Record(tenant string, tokens int, usd float64) {
	if !m.enabled || tenant == "" {
		return
	}
	if m.redis != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		key := fmt.Sprintf("budget:%s:%s", tenant, monthKey())
		pipe := m.redis.Pipeline()
		pipe.HIncrBy(ctx, key, "tokens", int64(tokens))
		pipe.HIncrByFloat(ctx, key, "usd", usd)
		pipe.Expire(ctx, key, 35*24*time.Hour)
		_, _ = pipe.Exec(ctx)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.mem[tenant]
	if u == nil || u.month != monthKey() {
		u = &usage{month: monthKey()}
		m.mem[tenant] = u
	}
	u.tokens += tokens
	u.usd += usd
}
