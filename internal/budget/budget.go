package budget

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/pricing"
	"github.com/fcordero/llm-api-gateway/internal/redisstore"
	"github.com/redis/go-redis/v9"
)

const (
	defaultCostPerTokenUSD = 0.00001
	budgetTTLSeconds       = 35 * 24 * 60 * 60

	defaultReservationTTL = 10 * time.Minute
	memPruneInterval      = 30 * time.Second
)

// ErrUnavailable means the distributed budget store could not make a
// decision. Budget enforcement fails closed so a Redis outage cannot silently
// turn a hard quota into unlimited spend.
var ErrUnavailable = errors.New("budget backend unavailable")

// Options configures optional distributed-budget behavior. Zero values keep
// historical behavior: a Redis store configured but inactive falls back to
// in-process accounting, and reservations lease for ten minutes.
type Options struct {
	// Store is the replaceable Redis connection. It may be nil or inactive.
	Store redisstore.Store
	// RequireSharedStore makes Check/Reserve return ErrUnavailable when the
	// store is nil or inactive instead of falling back to process-local
	// accounting. Enable this whenever more than one replica enforces a budget:
	// per-process accounting multiplies the quota by the replica count.
	RequireSharedStore bool
	// ReservationTTL bounds how long a reservation without Commit/Cancel can
	// hold quota. Expired leases are reclaimed lazily and by Reconcile.
	ReservationTTL time.Duration
	// Now is an optional clock for tests and deterministic month rollover.
	Now func() time.Time
}

// Metrics exposes reservation hygiene counters. The counts are process-local;
// main can publish ExpiredReservations as an orphan gauge in a later wave.
type Metrics struct {
	ReconcileRuns       int64
	ExpiredReservations int64
	ExpiredTokens       int64
	ExpiredUSD          float64
	FailedCommits       int64
}

// Manager tracks monthly token/USD budgets per tenant. Redis operations use
// Lua so the quota check and reservation are one atomic decision. Without
// Redis, the same semantics are protected by the in-process mutex.
//
// Reservations are leases rather than permanent charges: a reservation that is
// never committed or cancelled expires after ReservationTTL and its quota is
// reclaimed. This keeps a failed Commit from burning a tenant's monthly limit.
type Manager struct {
	mu        sync.Mutex
	mem       map[string]*usage
	lastPrune time.Time

	configMu        sync.RWMutex
	redisStore      redisstore.Store
	enabled         bool
	tokens          int
	usd             float64
	costPerTokenUSD float64
	catalog         *pricing.Catalog
	requireShared   bool
	reservationTTL  time.Duration
	now             func() time.Time

	metricsMu sync.Mutex
	metrics   Metrics
}

type usage struct {
	tokens         int
	usd            float64
	reservedTokens int
	reservedUSD    float64
	leases         map[string]lease
	month          string // "2026-09" in UTC
}

type lease struct {
	tokens   int
	usd      float64
	deadline time.Time
}

// Reservation holds a provisional budget charge. It must be finalized with
// Commit or Cancel; both operations are idempotent. If neither happens, the
// lease expires and Reconcile (or the next reservation) reclaims the quota.
type Reservation struct {
	manager        *Manager
	tenant         string
	month          string
	id             string
	reservedTokens int
	reservedUSD    float64
	deadline       time.Time
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
	catalog := fallbackCatalog(costPerTokenUSD)
	return NewWithPricing(tokens, usd, costPerTokenUSD, redisClient, catalog)
}

// NewWithPricing creates a manager with an immutable provider/model catalog.
func NewWithPricing(tokens int, usd, costPerTokenUSD float64, redisClient *redis.Client, catalog *pricing.Catalog) *Manager {
	return NewConfigured(tokens > 0 || usd > 0, tokens, usd, costPerTokenUSD, redisClient, catalog)
}

// NewConfigured creates a manager even when disabled. Keeping the manager
// installed lets a config reload enable budgets without rebuilding handlers.
func NewConfigured(enabled bool, tokens int, usd, costPerTokenUSD float64, redisClient *redis.Client, catalog *pricing.Catalog) *Manager {
	return NewConfiguredWithStore(enabled, tokens, usd, costPerTokenUSD, redisstore.NewStatic(redisClient), catalog)
}

// NewConfiguredWithStore creates a manager backed by a replaceable Redis
// store. The store may be empty while the gateway uses its local fallback.
func NewConfiguredWithStore(enabled bool, tokens int, usd, costPerTokenUSD float64, store redisstore.Store, catalog *pricing.Catalog) *Manager {
	return NewConfiguredWithStoreOptions(enabled, tokens, usd, costPerTokenUSD, catalog, Options{Store: store})
}

// NewConfiguredWithStoreOptions is the constructor main should adopt for
// multi-replica deployments. Exact signature:
//
//	func NewConfiguredWithStoreOptions(enabled bool, tokens int, usd, costPerTokenUSD float64, catalog *pricing.Catalog, opts Options) *Manager
//
// Pass Options{Store: redisStore, RequireSharedStore: cfg.RateLimit.Budget.RequireSharedStore}.
func NewConfiguredWithStoreOptions(enabled bool, tokens int, usd, costPerTokenUSD float64, catalog *pricing.Catalog, opts Options) *Manager {
	if costPerTokenUSD < 0 {
		costPerTokenUSD = 0
	}
	if catalog == nil {
		catalog = fallbackCatalog(costPerTokenUSD)
	}
	if opts.ReservationTTL <= 0 {
		opts.ReservationTTL = defaultReservationTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Manager{
		mem:             make(map[string]*usage),
		redisStore:      opts.Store,
		enabled:         enabled,
		tokens:          tokens,
		usd:             usd,
		costPerTokenUSD: costPerTokenUSD,
		catalog:         catalog,
		requireShared:   opts.RequireSharedStore,
		reservationTTL:  opts.ReservationTTL,
		now:             opts.Now,
	}
}

// fallbackCatalog builds the unknown-model catalog from a per-token fallback.
// Using NewCatalogWithFallbackPerToken avoids the historical unit bug where
// cost_per_token_usd was interpreted as USD per million tokens.
func fallbackCatalog(costPerTokenUSD float64) *pricing.Catalog {
	catalog, _ := pricing.NewCatalogWithFallbackPerToken(nil, costPerTokenUSD, pricing.DefaultCurrency, "", pricing.UnknownUseFallback)
	return catalog
}

// SetRedisStore swaps the distributed budget backend without replacing the
// manager used by request handlers.
func (m *Manager) SetRedisStore(store redisstore.Store) {
	m.configMu.Lock()
	m.redisStore = store
	m.configMu.Unlock()
}

// SetRequireSharedStore toggles fail-closed behavior at runtime (config reload).
func (m *Manager) SetRequireSharedStore(require bool) {
	m.configMu.Lock()
	m.requireShared = require
	m.configMu.Unlock()
}

// SetReservationTTL changes the lease duration for future reservations.
func (m *Manager) SetReservationTTL(ttl time.Duration) {
	if ttl <= 0 {
		ttl = defaultReservationTTL
	}
	m.configMu.Lock()
	m.reservationTTL = ttl
	m.configMu.Unlock()
}

func (m *Manager) redisStoreSnapshot() redisstore.Store {
	m.configMu.RLock()
	store := m.redisStore
	m.configMu.RUnlock()
	return store
}

func (m *Manager) requireSharedSnapshot() bool {
	m.configMu.RLock()
	require := m.requireShared
	m.configMu.RUnlock()
	return require
}

func (m *Manager) reservationTTLSnapshot() time.Duration {
	m.configMu.RLock()
	ttl := m.reservationTTL
	m.configMu.RUnlock()
	if ttl <= 0 {
		return defaultReservationTTL
	}
	return ttl
}

func (m *Manager) clock() time.Time {
	m.configMu.RLock()
	now := m.now
	m.configMu.RUnlock()
	if now == nil {
		return time.Now()
	}
	return now()
}

// sharedStore returns the active store, or ErrUnavailable when a shared store
// is required but unavailable.
func (m *Manager) sharedStore() (redisstore.Store, bool, error) {
	store := m.redisStoreSnapshot()
	if store != nil && store.Active() {
		return store, true, nil
	}
	if m.requireSharedSnapshot() {
		return nil, false, fmt.Errorf("%w: shared budget store is required but inactive", ErrUnavailable)
	}
	return store, false, nil
}

// UpdateConfig atomically swaps budget limits and pricing. Existing monthly
// usage remains intact, so tightening a limit cannot erase already consumed
// quota and disabling/re-enabling preserves the current month accounting.
func (m *Manager) UpdateConfig(enabled bool, tokens int, usd, costPerTokenUSD float64, catalog *pricing.Catalog) {
	if costPerTokenUSD < 0 {
		costPerTokenUSD = 0
	}
	if catalog == nil {
		catalog = fallbackCatalog(costPerTokenUSD)
	}
	m.configMu.Lock()
	m.enabled = enabled
	m.tokens = tokens
	m.usd = usd
	m.costPerTokenUSD = costPerTokenUSD
	m.catalog = catalog
	m.configMu.Unlock()
}

// monthKey returns the UTC month. Local-time months split quota at the
// process's timezone and make replicas in different zones disagree.
func monthKey() string {
	return monthKeyAt(time.Now())
}

func monthKeyAt(t time.Time) string {
	return t.UTC().Format("2006-01")
}

// CostForTokens returns the configured fallback USD estimate.
func (m *Manager) CostForTokens(tokens int) float64 {
	if tokens <= 0 {
		return 0
	}
	m.configMu.RLock()
	cost := m.costPerTokenUSD
	m.configMu.RUnlock()
	return float64(tokens) * cost
}

func (m *Manager) CostForChat(provider, model string, promptTokens, completionTokens int) (pricing.Estimate, error) {
	catalog := m.catalogSnapshot()
	if catalog != nil {
		return catalog.Chat(provider, model, promptTokens, completionTokens)
	}
	return pricing.Estimate{USD: m.CostForTokens(promptTokens + completionTokens), Known: false, Currency: pricing.DefaultCurrency}, nil
}

// CostForChatWithCacheTokens prices a completed request using the provider's
// reported cache read/write token split. It falls back to the configured
// per-token cost when no catalog is available.
func (m *Manager) CostForChatWithCacheTokens(provider, model string, promptTokens, completionTokens, cacheReadTokens, cacheWriteTokens int) (pricing.Estimate, error) {
	catalog := m.catalogSnapshot()
	if catalog != nil {
		return catalog.ChatWithCacheTokens(provider, model, promptTokens, completionTokens, cacheReadTokens, cacheWriteTokens)
	}
	return pricing.Estimate{USD: m.CostForTokens(promptTokens + completionTokens), Known: false, Currency: pricing.DefaultCurrency}, nil
}

func (m *Manager) EstimateChat(model string, promptTokens, completionTokens int) (pricing.Estimate, error) {
	catalog := m.catalogSnapshot()
	if catalog != nil {
		return catalog.EstimateChat(model, promptTokens, completionTokens)
	}
	return pricing.Estimate{USD: m.CostForTokens(promptTokens + completionTokens), Known: false, Currency: pricing.DefaultCurrency}, nil
}

func (m *Manager) CostForEmbedding(provider, model string, tokens int) (pricing.Estimate, error) {
	catalog := m.catalogSnapshot()
	if catalog != nil {
		return catalog.Embedding(provider, model, tokens)
	}
	return pricing.Estimate{USD: m.CostForTokens(tokens), Known: false, Currency: pricing.DefaultCurrency}, nil
}

func (m *Manager) catalogSnapshot() *pricing.Catalog {
	m.configMu.RLock()
	catalog := m.catalog
	m.configMu.RUnlock()
	return catalog
}

func (m *Manager) limitsSnapshot() (bool, int, float64) {
	m.configMu.RLock()
	defer m.configMu.RUnlock()
	return m.enabled, m.tokens, m.usd
}

// Check returns an error if the tenant has no capacity left for another
// request. New request paths should prefer Reserve so concurrent in-flight
// requests are accounted for before they call a provider.
func (m *Manager) Check(tenant string) error {
	enabled, tokenLimit, usdLimit := m.limitsSnapshot()
	if !enabled || tenant == "" {
		return nil
	}
	now := m.clock()
	store, shared, err := m.sharedStore()
	if err != nil {
		return err
	}
	if shared {
		return m.checkRedis(store, tenant, monthKeyAt(now), now, tokenLimit, usdLimit)
	}
	return m.checkMem(tenant, monthKeyAt(now), now, tokenLimit, usdLimit)
}

// Reserve atomically reserves an estimated amount before an upstream call.
// The reservation must be committed with actual usage or cancelled on failure.
func (m *Manager) Reserve(tenant string, tokens int, usd float64) (*Reservation, error) {
	enabled, tokenLimit, usdLimit := m.limitsSnapshot()
	if !enabled || tenant == "" {
		return nil, nil
	}
	if tokens < 0 {
		tokens = 0
	}
	if usd < 0 {
		usd = 0
	}
	now := m.clock()
	month := monthKeyAt(now)
	store, shared, err := m.sharedStore()
	if err != nil {
		return nil, err
	}
	id := newReservationID()
	ttl := m.reservationTTLSnapshot()
	if shared {
		if err := m.reserveRedis(store, tenant, month, id, tokens, usd, now, ttl, tokenLimit, usdLimit); err != nil {
			return nil, err
		}
	} else if err := m.reserveMem(tenant, month, id, tokens, usd, now, ttl, tokenLimit, usdLimit); err != nil {
		return nil, err
	}
	return &Reservation{
		manager:        m,
		tenant:         tenant,
		month:          month,
		id:             id,
		reservedTokens: tokens,
		reservedUSD:    usd,
		deadline:       now.Add(ttl),
	}, nil
}

// Commit replaces the estimate with actual usage. If actual usage is larger,
// the adjustment is still recorded and an over-budget error is returned for
// observability; the already-completed upstream request cannot be undone.
func (r *Reservation) Commit(actualTokens int, actualUSD float64) error {
	return r.commit(actualTokens, actualUSD, 1)
}

// CommitWithRetry retries Commit when the shared store is temporarily
// unavailable. attempts includes the first call and is capped at five. Quota
// errors are never retried. If every attempt fails, the reservation lease
// remains and is reclaimed by its TTL / Reconcile, so quota is not burned.
func (r *Reservation) CommitWithRetry(actualTokens int, actualUSD float64, attempts int) error {
	return r.commit(actualTokens, actualUSD, attempts)
}

func (r *Reservation) commit(actualTokens int, actualUSD float64, attempts int) error {
	if r == nil {
		return nil
	}
	if actualTokens < 0 {
		actualTokens = 0
	}
	if actualUSD < 0 {
		actualUSD = 0
	}
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 5 {
		attempts = 5
	}
	r.once.Do(func() {
		var err error
		delay := 25 * time.Millisecond
		for attempt := 0; attempt < attempts; attempt++ {
			err = r.manager.commitReservation(r, actualTokens, actualUSD)
			if err == nil || !errors.Is(err, ErrUnavailable) {
				break
			}
			if attempt+1 < attempts {
				time.Sleep(delay)
				if delay < 500*time.Millisecond {
					delay *= 2
				}
			}
		}
		if err != nil && errors.Is(err, ErrUnavailable) {
			r.manager.recordFailedCommit()
		}
		r.err = err
	})
	return r.err
}

// Cancel releases a reservation when no billable provider work completed.
// It is best effort: if the store is unavailable the lease expires on its own.
func (r *Reservation) Cancel() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.err = r.manager.cancelReservation(r)
		if r.err != nil && errors.Is(r.err, ErrUnavailable) {
			r.manager.recordFailedCommit()
		}
	})
}

// Adjust records already-known usage. It is the error-returning counterpart of
// Record and is what Record uses; callers that need to observe a fail-closed
// ErrUnavailable should call Adjust directly.
func (m *Manager) Adjust(tenant string, tokens int, usd float64) error {
	enabled, _, _ := m.limitsSnapshot()
	if !enabled || tenant == "" {
		return nil
	}
	now := m.clock()
	month := monthKeyAt(now)
	store, shared, err := m.sharedStore()
	if err != nil {
		return err
	}
	if shared {
		return m.adjustRedis(store, tenant, month, now, tokens, usd)
	}
	return m.adjustMem(tenant, month, tokens, usd, now)
}

// Record remains for callers that already have actual usage but cannot use a
// reservation. New request paths should use Reserve/Commit instead.
func (m *Manager) Record(tenant string, tokens int, usd float64) {
	_ = m.Adjust(tenant, tokens, usd)
}

// ReconcileResult reports the reservations reclaimed in one Reconcile pass.
type ReconcileResult struct {
	Reservations int
	Tokens       int
	USD          float64
}

// Reconcile reclaims reservations whose lease expired without Commit/Cancel.
// It is safe to call periodically from a background job and also runs lazily
// on Check/Reserve. The result reports what this pass reclaimed; cumulative
// counters remain available through MetricsSnapshot.
func (m *Manager) Reconcile(tenant string) (ReconcileResult, error) {
	var result ReconcileResult
	enabled, _, _ := m.limitsSnapshot()
	if !enabled || tenant == "" {
		return result, nil
	}
	now := m.clock()
	month := monthKeyAt(now)
	store, shared, err := m.sharedStore()
	if err != nil {
		return result, err
	}
	if shared {
		result, err = m.reconcileRedis(store, tenant, month, now)
	} else {
		m.mu.Lock()
		if u := m.mem[memKey(tenant, month)]; u != nil {
			result = m.pruneUsageLocked(u, now)
			m.deleteIfIdleLocked(tenant, month, u)
		}
		m.mu.Unlock()
	}
	m.recordReconcile()
	return result, err
}

// MetricsSnapshot returns reservation hygiene counters for metrics wiring.
func (m *Manager) MetricsSnapshot() Metrics {
	m.metricsMu.Lock()
	defer m.metricsMu.Unlock()
	return m.metrics
}

func (m *Manager) recordExpired(tokens int, usd float64) {
	m.metricsMu.Lock()
	m.metrics.ExpiredReservations++
	m.metrics.ExpiredTokens += int64(tokens)
	m.metrics.ExpiredUSD += usd
	m.metricsMu.Unlock()
}

func (m *Manager) recordFailedCommit() {
	m.metricsMu.Lock()
	m.metrics.FailedCommits++
	m.metricsMu.Unlock()
}

func (m *Manager) recordReconcile() {
	m.metricsMu.Lock()
	m.metrics.ReconcileRuns++
	m.metricsMu.Unlock()
}

func (m *Manager) commitReservation(r *Reservation, actualTokens int, actualUSD float64) error {
	store, shared, err := m.sharedStore()
	if err != nil {
		return err
	}
	if shared {
		return m.commitRedis(store, r.tenant, r.month, r.id, actualTokens, actualUSD)
	}
	return m.commitMem(r, actualTokens, actualUSD)
}

func (m *Manager) cancelReservation(r *Reservation) error {
	store, shared, err := m.sharedStore()
	if err != nil {
		return err
	}
	if shared {
		return m.cancelRedis(store, r.tenant, r.month, r.id)
	}
	m.mu.Lock()
	if u := m.mem[memKey(r.tenant, r.month)]; u != nil {
		m.removeLeaseLocked(u, r.id)
		m.deleteIfIdleLocked(r.tenant, r.month, u)
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) checkMem(tenant, month string, now time.Time, tokenLimit int, usdLimit float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneMemLocked(now, month)
	u := m.mem[memKey(tenant, month)]
	if u == nil {
		return nil
	}
	m.pruneUsageLocked(u, now)
	return admissionQuotaError(tokenLimit, usdLimit, u.tokens+u.reservedTokens, u.usd+u.reservedUSD)
}

func (m *Manager) checkRedis(store redisstore.Store, tenant, month string, now time.Time, tokenLimit int, usdLimit float64) error {
	var raw interface{}
	err := store.WithClient(func(client *redis.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		var err error
		raw, err = budgetSnapshotScript.Run(ctx, client,
			[]string{budgetKey(tenant, month), leaseKey(tenant, month)},
			now.Unix(), budgetTTLSeconds).Result()
		return err
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	val, ok := raw.([]interface{})
	if !ok || len(val) < 2 {
		return fmt.Errorf("%w: unexpected snapshot response %T", ErrUnavailable, raw)
	}
	tokens := parseInt(val, 0)
	usd := parseFloat(val, 1)
	return admissionQuotaError(tokenLimit, usdLimit, tokens, usd)
}

func (m *Manager) reserveMem(tenant, month, id string, tokens int, usd float64, now time.Time, ttl time.Duration, tokenLimit int, usdLimit float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneMemLocked(now, month)
	key := memKey(tenant, month)
	u := m.mem[key]
	if u == nil {
		u = &usage{month: month, leases: make(map[string]lease)}
		m.mem[key] = u
	}
	m.pruneUsageLocked(u, now)
	if err := quotaError(tokenLimit, usdLimit, u.tokens+u.reservedTokens+tokens, u.usd+u.reservedUSD+usd); err != nil {
		m.deleteIfIdleLocked(tenant, month, u)
		return err
	}
	u.reservedTokens += tokens
	u.reservedUSD += usd
	u.leases[id] = lease{tokens: tokens, usd: usd, deadline: now.Add(ttl)}
	return nil
}

func (m *Manager) reserveRedis(store redisstore.Store, tenant, month, id string, tokens int, usd float64, now time.Time, ttl time.Duration, tokenLimit int, usdLimit float64) error {
	var result interface{}
	err := store.WithClient(func(client *redis.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		var err error
		result, err = budgetReserveLeaseScript.Run(ctx, client,
			[]string{budgetKey(tenant, month), leaseKey(tenant, month)},
			tokenLimit, usdLimit, tokens, usd, now.Unix(), int64(ttl.Seconds()), id, budgetTTLSeconds).Result()
		return err
	})
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

func (m *Manager) commitMem(r *Reservation, actualTokens int, actualUSD float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := memKey(r.tenant, r.month)
	u := m.mem[key]
	if u == nil {
		if actualTokens == 0 && actualUSD == 0 {
			return nil
		}
		u = &usage{month: r.month, leases: make(map[string]lease)}
		m.mem[key] = u
	}
	m.removeLeaseLocked(u, r.id)
	u.tokens += actualTokens
	u.usd += actualUSD
	if u.tokens < 0 {
		u.tokens = 0
	}
	if u.usd < 0 {
		u.usd = 0
	}
	_, tokenLimit, usdLimit := m.limitsSnapshot()
	m.deleteIfIdleLocked(r.tenant, r.month, u)
	return quotaError(tokenLimit, usdLimit, u.tokens, u.usd)
}

func (m *Manager) adjustMem(tenant, month string, deltaTokens int, deltaUSD float64, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneMemLocked(now, month)
	key := memKey(tenant, month)
	u := m.mem[key]
	if u == nil {
		if deltaTokens <= 0 && deltaUSD <= 0 {
			return nil
		}
		u = &usage{month: month, leases: make(map[string]lease)}
		m.mem[key] = u
	}
	m.pruneUsageLocked(u, now)
	u.tokens += deltaTokens
	u.usd += deltaUSD
	if u.tokens < 0 {
		u.tokens = 0
	}
	if u.usd < 0 {
		u.usd = 0
	}
	_, tokenLimit, usdLimit := m.limitsSnapshot()
	m.deleteIfIdleLocked(tenant, month, u)
	return quotaError(tokenLimit, usdLimit, u.tokens, u.usd)
}

func (m *Manager) commitRedis(store redisstore.Store, tenant, month, id string, actualTokens int, actualUSD float64) error {
	_, tokenLimit, usdLimit := m.limitsSnapshot()
	var result interface{}
	err := store.WithClient(func(client *redis.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		var err error
		result, err = budgetCommitLeaseScript.Run(ctx, client,
			[]string{budgetKey(tenant, month), leaseKey(tenant, month)},
			id, actualTokens, actualUSD, tokenLimit, usdLimit, budgetTTLSeconds).Result()
		return err
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	over, ok := result.(int64)
	if !ok {
		return fmt.Errorf("%w: unexpected commit response %T", ErrUnavailable, result)
	}
	if over == 1 {
		return fmt.Errorf("insufficient_quota: monthly budget exceeded after adjustment")
	}
	return nil
}

func (m *Manager) cancelRedis(store redisstore.Store, tenant, month, id string) error {
	err := store.WithClient(func(client *redis.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		return budgetCancelLeaseScript.Run(ctx, client,
			[]string{budgetKey(tenant, month), leaseKey(tenant, month)}, id).Err()
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

func (m *Manager) adjustRedis(store redisstore.Store, tenant, month string, now time.Time, deltaTokens int, deltaUSD float64) error {
	_, tokenLimit, usdLimit := m.limitsSnapshot()
	var result interface{}
	err := store.WithClient(func(client *redis.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		var err error
		result, err = budgetAdjustScript.Run(ctx, client, []string{budgetKey(tenant, month)},
			deltaTokens, deltaUSD, tokenLimit, usdLimit, budgetTTLSeconds).Result()
		return err
	})
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

func (m *Manager) reconcileRedis(store redisstore.Store, tenant, month string, now time.Time) (ReconcileResult, error) {
	var result ReconcileResult
	var raw interface{}
	err := store.WithClient(func(client *redis.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var err error
		raw, err = budgetReconcileScript.Run(ctx, client,
			[]string{leaseKey(tenant, month)}, now.Unix()).Result()
		return err
	})
	if err != nil {
		return result, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	values, ok := raw.([]interface{})
	if !ok || len(values) < 3 {
		return result, fmt.Errorf("%w: unexpected reconcile response %T", ErrUnavailable, raw)
	}
	result.Reservations = parseInt(values, 0)
	result.Tokens = parseInt(values, 1)
	result.USD = parseFloat(values, 2)
	if result.Reservations > 0 {
		m.metricsMu.Lock()
		m.metrics.ExpiredReservations += int64(result.Reservations)
		m.metrics.ExpiredTokens += int64(result.Tokens)
		m.metrics.ExpiredUSD += result.USD
		m.metricsMu.Unlock()
	}
	return result, nil
}

func (m *Manager) pruneUsageLocked(u *usage, now time.Time) ReconcileResult {
	var result ReconcileResult
	for id, l := range u.leases {
		if l.deadline.After(now) {
			continue
		}
		delete(u.leases, id)
		u.reservedTokens -= l.tokens
		u.reservedUSD -= l.usd
		result.Reservations++
		result.Tokens += l.tokens
		result.USD += l.usd
		m.recordExpired(l.tokens, l.usd)
	}
	if u.reservedTokens < 0 {
		u.reservedTokens = 0
	}
	if u.reservedUSD < 0 {
		u.reservedUSD = 0
	}
	return result
}

func (m *Manager) removeLeaseLocked(u *usage, id string) {
	l, ok := u.leases[id]
	if !ok {
		return
	}
	delete(u.leases, id)
	u.reservedTokens -= l.tokens
	u.reservedUSD -= l.usd
	if u.reservedTokens < 0 {
		u.reservedTokens = 0
	}
	if u.reservedUSD < 0 {
		u.reservedUSD = 0
	}
}

// pruneMemLocked drops accounting for previous months and idle tenants. Budgets
// reset monthly, so retaining old months only grows the map forever.
func (m *Manager) pruneMemLocked(now time.Time, currentMonth string) {
	if !m.lastPrune.IsZero() && now.Sub(m.lastPrune) < memPruneInterval && len(m.mem) < 4096 {
		return
	}
	m.lastPrune = now
	for key, u := range m.mem {
		if u.month != currentMonth {
			delete(m.mem, key)
			continue
		}
		m.pruneUsageLocked(u, now)
		if u.tokens == 0 && u.usd == 0 && len(u.leases) == 0 {
			delete(m.mem, key)
		}
	}
}

func (m *Manager) deleteIfIdleLocked(tenant, month string, u *usage) {
	if u.tokens != 0 || u.usd != 0 || len(u.leases) != 0 {
		return
	}
	key := memKey(tenant, month)
	if m.mem[key] == u {
		delete(m.mem, key)
	}
}

// quotaError reports whether actual/projected usage exceeds the limit. The
// same comparator is used for reservations, commits and recorded usage:
// spending exactly up to the limit is allowed, the next unit is not.
func quotaError(tokenLimit int, usdLimit float64, tokens int, usd float64) error {
	if tokenLimit > 0 && tokens > tokenLimit {
		return fmt.Errorf("insufficient_quota: monthly token budget exceeded (%d/%d)", tokens, tokenLimit)
	}
	if usdLimit > 0 && usd > usdLimit {
		return fmt.Errorf("insufficient_quota: monthly USD budget exceeded (%.2f/%.2f)", usd, usdLimit)
	}
	return nil
}

// admissionQuotaError treats a fully consumed budget as exhausted even though
// usage equals the limit exactly: Check cannot know the next request's size,
// so it requires at least one token (or a sliver of USD) of headroom.
func admissionQuotaError(tokenLimit int, usdLimit float64, tokens int, usd float64) error {
	if tokenLimit > 0 && tokens+1 > tokenLimit {
		return fmt.Errorf("insufficient_quota: monthly token budget exceeded (%d/%d)", tokens, tokenLimit)
	}
	if usdLimit > 0 && usd+1e-9 > usdLimit {
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

func leaseKey(tenant, month string) string {
	sum := sha256.Sum256([]byte(tenant))
	return "budget:lease:" + hex.EncodeToString(sum[:]) + ":" + month
}

func newReservationID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf[:])
}

// budgetReserveLeaseScript atomically checks committed usage plus live leases
// and registers a new lease. Expired leases are removed in the same call so an
// abandoned Commit cannot hold quota for the rest of the month.
var budgetReserveLeaseScript = redis.NewScript(`
local usage = KEYS[1]
local leases = KEYS[2]
local token_limit = tonumber(ARGV[1]) or 0
local usd_limit = tonumber(ARGV[2]) or 0
local reserve_tokens = tonumber(ARGV[3]) or 0
local reserve_usd = tonumber(ARGV[4]) or 0
local now = tonumber(ARGV[5])
local lease_ttl = tonumber(ARGV[6])
local id = ARGV[7]
local usage_ttl = tonumber(ARGV[8])

local pending_tokens = 0
local pending_usd = 0
local flat = redis.call("HGETALL", leases)
for i = 1, #flat, 2 do
  local t, u, deadline = string.match(flat[i+1], "^([^|]+)|([^|]+)|([^|]+)$")
  deadline = tonumber(deadline)
  if deadline == nil or deadline <= now then
    redis.call("HDEL", leases, flat[i])
  else
    pending_tokens = pending_tokens + (tonumber(t) or 0)
    pending_usd = pending_usd + (tonumber(u) or 0)
  end
end

local tokens = tonumber(redis.call("HGET", usage, "tokens")) or 0
local usd = tonumber(redis.call("HGET", usage, "usd")) or 0
if token_limit > 0 and tokens + pending_tokens + reserve_tokens > token_limit then return 0 end
if usd_limit > 0 and usd + pending_usd + reserve_usd > usd_limit then return 0 end

redis.call("HSET", leases, id, tostring(reserve_tokens) .. "|" .. tostring(reserve_usd) .. "|" .. tostring(now + lease_ttl))
redis.call("EXPIRE", leases, usage_ttl)
if redis.call("EXISTS", usage) == 1 then redis.call("EXPIRE", usage, usage_ttl) end
return 1
`)

var budgetSnapshotScript = redis.NewScript(`
local usage = KEYS[1]
local leases = KEYS[2]
local now = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])

local pending_tokens = 0
local pending_usd = 0
local flat = redis.call("HGETALL", leases)
for i = 1, #flat, 2 do
  local t, u, deadline = string.match(flat[i+1], "^([^|]+)|([^|]+)|([^|]+)$")
  deadline = tonumber(deadline)
  if deadline == nil or deadline <= now then
    redis.call("HDEL", leases, flat[i])
  else
    pending_tokens = pending_tokens + (tonumber(t) or 0)
    pending_usd = pending_usd + (tonumber(u) or 0)
  end
end

local tokens = tonumber(redis.call("HGET", usage, "tokens")) or 0
local usd = tonumber(redis.call("HGET", usage, "usd")) or 0
if redis.call("EXISTS", usage) == 1 then redis.call("EXPIRE", usage, ttl) end
if redis.call("EXISTS", leases) == 1 then redis.call("EXPIRE", leases, ttl) end
return {tokens + pending_tokens, tostring(usd + pending_usd)}
`)

var budgetCommitLeaseScript = redis.NewScript(`
local usage = KEYS[1]
local leases = KEYS[2]
local id = ARGV[1]
local actual_tokens = tonumber(ARGV[2]) or 0
local actual_usd = tonumber(ARGV[3]) or 0
local token_limit = tonumber(ARGV[4]) or 0
local usd_limit = tonumber(ARGV[5]) or 0
local ttl = tonumber(ARGV[6])

redis.call("HDEL", leases, id)
local tokens = (tonumber(redis.call("HGET", usage, "tokens")) or 0) + actual_tokens
local usd = (tonumber(redis.call("HGET", usage, "usd")) or 0) + actual_usd
if tokens < 0 then tokens = 0 end
if usd < 0 then usd = 0 end
redis.call("HSET", usage, "tokens", tokens, "usd", usd)
redis.call("EXPIRE", usage, ttl)
if redis.call("EXISTS", leases) == 1 then redis.call("EXPIRE", leases, ttl) end
if token_limit > 0 and tokens > token_limit then return 1 end
if usd_limit > 0 and usd > usd_limit then return 1 end
return 0
`)

var budgetCancelLeaseScript = redis.NewScript(`
redis.call("HDEL", KEYS[2], ARGV[1])
return 1
`)

var budgetReconcileScript = redis.NewScript(`
local leases = KEYS[1]
local now = tonumber(ARGV[1])
local count = 0
local tokens = 0
local usd = 0
local flat = redis.call("HGETALL", leases)
for i = 1, #flat, 2 do
  local t, u, deadline = string.match(flat[i+1], "^([^|]+)|([^|]+)|([^|]+)$")
  deadline = tonumber(deadline)
  if deadline == nil or deadline <= now then
    count = count + 1
    tokens = tokens + (tonumber(t) or 0)
    usd = usd + (tonumber(u) or 0)
    redis.call("HDEL", leases, flat[i])
  end
end
return {count, tokens, tostring(usd)}
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
