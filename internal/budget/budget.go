package budget

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultCostPerTokenUSD = 0.00001
	budgetTTLSeconds       = 35 * 24 * 60 * 60
)

// ErrUnavailable means the distributed budget store could not make a
// decision. Budget enforcement fails closed so a Redis outage cannot silently
// turn a hard quota into unlimited spend.
var ErrUnavailable = errors.New("budget backend unavailable")

// Manager tracks monthly token/USD budgets per tenant. Redis operations use
// Lua so the quota check and reservation are one atomic decision.
// Without Redis, the same semantics are protected by the in-process mutex.
type Manager struct {
	mu              sync.Mutex
	mem             map[string]*usage
	redis           *redis.Client
	enabled         bool
	tokens          int
	usd             float64
	costPerTokenUSD float64
}

type usage struct {
	tokens int
	usd    float64
	month  string // "2026-09"
}

// Reservation holds a provisional budget charge. It must be finalized with
// Commit or Cancel; both operations are idempotent.
type Reservation struct {
	manager        *Manager
	tenant         string
	month          string
	reservedTokens int
	reservedUSD    float64
	once           sync.Once
	err            error
}

// New creates a budget manager using the default estimated cost per token.
func New(tokens int, usd float64, redisClient *redis.Client) *Manager {
	return NewWithCost(tokens, usd, defaultCostPerTokenUSD, redisClient)
}

// NewWithCost creates a manager with a configurable fallback cost estimate.
// Provider/model-specific pricing can later replace this estimate without
// changing reservation semantics.
func NewWithCost(tokens int, usd, costPerTokenUSD float64, redisClient *redis.Client) *Manager {
	if costPerTokenUSD < 0 {
		costPerTokenUSD = 0
	}
	return &Manager{
		mem:             make(map[string]*usage),
		redis:           redisClient,
		enabled:         tokens > 0 || usd > 0,
		tokens:          tokens,
		usd:             usd,
		costPerTokenUSD: costPerTokenUSD,
	}
}

func monthKey() string {
	return time.Now().Format("2006-01")
}

// CostForTokens returns the configured fallback USD estimate.
func (m *Manager) CostForTokens(tokens int) float64 {
	if tokens <= 0 {
		return 0
	}
	return float64(tokens) * m.costPerTokenUSD
}

// Check returns an error if the tenant exceeds its monthly budget. New
// request paths should prefer Reserve so concurrent in-flight requests are
// accounted for before they call a provider.
func (m *Manager) Check(tenant string) error {
	if !m.enabled || tenant == "" {
		return nil
	}
	if m.redis != nil {
		return m.checkRedis(tenant)
	}
	return m.checkMem(tenant, monthKey())
}

// Reserve atomically reserves an estimated amount before an upstream call.
// The reservation must be committed with actual usage or cancelled on failure.
func (m *Manager) Reserve(tenant string, tokens int, usd float64) (*Reservation, error) {
	if !m.enabled || tenant == "" {
		return nil, nil
	}
	if tokens < 0 {
		tokens = 0
	}
	if usd < 0 {
		usd = 0
	}
	month := monthKey()
	if m.redis != nil {
		if err := m.reserveRedis(tenant, month, tokens, usd); err != nil {
			return nil, err
		}
	} else if err := m.reserveMem(tenant, month, tokens, usd); err != nil {
		return nil, err
	}
	return &Reservation{
		manager:        m,
		tenant:         tenant,
		month:          month,
		reservedTokens: tokens,
		reservedUSD:    usd,
	}, nil
}

// Commit replaces the estimate with actual usage. If actual usage is larger,
// the adjustment is still recorded and an over-budget error is returned for
// observability; the already-completed upstream request cannot be undone.
func (r *Reservation) Commit(actualTokens int, actualUSD float64) error {
	if r == nil {
		return nil
	}
	if actualTokens < 0 {
		actualTokens = 0
	}
	if actualUSD < 0 {
		actualUSD = 0
	}
	r.once.Do(func() {
		r.err = r.manager.adjust(
			r.tenant,
			r.month,
			actualTokens-r.reservedTokens,
			actualUSD-r.reservedUSD,
		)
	})
	return r.err
}

// Cancel releases a reservation when no billable provider work completed.
func (r *Reservation) Cancel() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.err = r.manager.adjust(r.tenant, r.month, -r.reservedTokens, -r.reservedUSD)
	})
}

func (m *Manager) checkMem(tenant, month string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.mem[memKey(tenant, month)]
	if u == nil {
		return nil
	}
	return quotaError(m.tokens, m.usd, u.tokens, u.usd)
}

func (m *Manager) checkRedis(tenant string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	key := budgetKey(tenant, monthKey())
	val, err := m.redis.HMGet(ctx, key, "tokens", "usd").Result()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	tokens := parseInt(val, 0)
	usd := parseFloat(val, 1)
	return quotaError(m.tokens, m.usd, tokens, usd)
}

func (m *Manager) reserveMem(tenant, month string, tokens int, usd float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memKey(tenant, month)
	u := m.mem[key]
	if u == nil {
		u = &usage{month: month}
		m.mem[key] = u
	}
	if err := reservationQuotaError(m.tokens, m.usd, u.tokens+tokens, u.usd+usd); err != nil {
		return err
	}
	u.tokens += tokens
	u.usd += usd
	return nil
}

func (m *Manager) reserveRedis(tenant, month string, tokens int, usd float64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := budgetReserveScript.Run(ctx, m.redis, []string{budgetKey(tenant, month)},
		m.tokens, m.usd, tokens, usd, budgetTTLSeconds).Result()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	allowed, ok := result.(int64)
	if !ok {
		return fmt.Errorf("%w: unexpected reserve response %T", ErrUnavailable, result)
	}
	if allowed != 1 {
		return fmt.Errorf("insufficient_quota: monthly budget reservation denied")
	}
	return nil
}

// Record remains for callers that already have actual usage but cannot use a
// reservation. New request paths should use Reserve/Commit instead.
func (m *Manager) Record(tenant string, tokens int, usd float64) {
	if !m.enabled || tenant == "" {
		return
	}
	_ = m.adjust(tenant, monthKey(), tokens, usd)
}

func (m *Manager) adjust(tenant, month string, deltaTokens int, deltaUSD float64) error {
	if m.redis != nil {
		return m.adjustRedis(tenant, month, deltaTokens, deltaUSD)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memKey(tenant, month)
	u := m.mem[key]
	if u == nil {
		if deltaTokens <= 0 && deltaUSD <= 0 {
			return nil
		}
		u = &usage{month: month}
		m.mem[key] = u
	}
	u.tokens += deltaTokens
	u.usd += deltaUSD
	if u.tokens < 0 {
		u.tokens = 0
	}
	if u.usd < 0 {
		u.usd = 0
	}
	return reservationQuotaError(m.tokens, m.usd, u.tokens, u.usd)
}

func (m *Manager) adjustRedis(tenant, month string, deltaTokens int, deltaUSD float64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := budgetAdjustScript.Run(ctx, m.redis, []string{budgetKey(tenant, month)},
		deltaTokens, deltaUSD, m.tokens, m.usd, budgetTTLSeconds).Result()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	over, ok := result.(int64)
	if !ok {
		return fmt.Errorf("%w: unexpected adjustment response %T", ErrUnavailable, result)
	}
	if over == 1 {
		return fmt.Errorf("insufficient_quota: monthly budget exceeded after adjustment")
	}
	return nil
}

func quotaError(tokenLimit int, usdLimit float64, tokens int, usd float64) error {
	if tokenLimit > 0 && tokens >= tokenLimit {
		return fmt.Errorf("insufficient_quota: monthly token budget exceeded (%d/%d)", tokens, tokenLimit)
	}
	if usdLimit > 0 && usd >= usdLimit {
		return fmt.Errorf("insufficient_quota: monthly USD budget exceeded (%.2f/%.2f)", usd, usdLimit)
	}
	return nil
}

func reservationQuotaError(tokenLimit int, usdLimit float64, tokens int, usd float64) error {
	if tokenLimit > 0 && tokens > tokenLimit {
		return fmt.Errorf("insufficient_quota: monthly token budget exceeded (%d/%d)", tokens, tokenLimit)
	}
	if usdLimit > 0 && usd > usdLimit {
		return fmt.Errorf("insufficient_quota: monthly USD budget exceeded (%.2f/%.2f)", usd, usdLimit)
	}
	return nil
}

func parseInt(values []interface{}, index int) int {
	if index >= len(values) || values[index] == nil {
		return 0
	}
	value, _ := strconv.Atoi(fmt.Sprintf("%v", values[index]))
	return value
}

func parseFloat(values []interface{}, index int) float64 {
	if index >= len(values) || values[index] == nil {
		return 0
	}
	value, _ := strconv.ParseFloat(fmt.Sprintf("%v", values[index]), 64)
	return value
}

func memKey(tenant, month string) string {
	return tenant + "\x00" + month
}

func budgetKey(tenant, month string) string {
	sum := sha256.Sum256([]byte(tenant))
	return "budget:" + hex.EncodeToString(sum[:]) + ":" + month
}

var budgetReserveScript = redis.NewScript(`
local key = KEYS[1]
local token_limit = tonumber(ARGV[1]) or 0
local usd_limit = tonumber(ARGV[2]) or 0
local reserve_tokens = tonumber(ARGV[3]) or 0
local reserve_usd = tonumber(ARGV[4]) or 0
local ttl = tonumber(ARGV[5])
local tokens = tonumber(redis.call("HGET", key, "tokens")) or 0
local usd = tonumber(redis.call("HGET", key, "usd")) or 0

if token_limit > 0 and tokens + reserve_tokens > token_limit then return 0 end
if usd_limit > 0 and usd + reserve_usd > usd_limit then return 0 end

redis.call("HINCRBY", key, "tokens", reserve_tokens)
redis.call("HINCRBYFLOAT", key, "usd", reserve_usd)
redis.call("EXPIRE", key, ttl)
return 1
`)

var budgetAdjustScript = redis.NewScript(`
local key = KEYS[1]
local delta_tokens = tonumber(ARGV[1]) or 0
local delta_usd = tonumber(ARGV[2]) or 0
local token_limit = tonumber(ARGV[3]) or 0
local usd_limit = tonumber(ARGV[4]) or 0
local ttl = tonumber(ARGV[5])
local tokens = (tonumber(redis.call("HGET", key, "tokens")) or 0) + delta_tokens
local usd = (tonumber(redis.call("HGET", key, "usd")) or 0) + delta_usd
if tokens < 0 then tokens = 0 end
if usd < 0 then usd = 0 end
redis.call("HSET", key, "tokens", tokens, "usd", usd)
redis.call("EXPIRE", key, ttl)
local over = 0
if token_limit > 0 and tokens > token_limit then over = 1 end
if usd_limit > 0 and usd > usd_limit then over = 1 end
return over
`)
