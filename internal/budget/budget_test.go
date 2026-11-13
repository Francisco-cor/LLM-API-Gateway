package budget

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/pricing"
	"github.com/fcordero/llm-api-gateway/internal/redisstore"
	"github.com/redis/go-redis/v9"
)

func TestManagerPricingAndHotReload(t *testing.T) {
	catalog, err := pricing.NewCatalog([]pricing.Rule{{
		Provider:                  "openai",
		Model:                     "gpt-4o",
		InputPerMillionTokensUSD:  2,
		OutputPerMillionTokensUSD: 4,
	}}, 0.01, "USD", "v1", pricing.UnknownUseFallback)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewConfigured(false, 100, 10, 0.01, nil, catalog)
	if reservation, err := mgr.Reserve("tenant-a", 1, 1); err != nil || reservation != nil {
		t.Fatalf("disabled manager reserve = %v, %v", reservation, err)
	}

	mgr.UpdateConfig(true, 100, 10, 0.01, catalog)
	estimate, err := mgr.CostForChat("openai", "gpt-4o", 1_000_000, 1_000_000)
	if err != nil || estimate.USD != 6 || !estimate.Known || estimate.Version != "v1" {
		t.Fatalf("chat estimate = %+v, err=%v", estimate, err)
	}
	reservation, err := mgr.Reserve("tenant-a", 100, 6)
	if err != nil || reservation == nil {
		t.Fatalf("reserve after enable = %v, %v", reservation, err)
	}
	reservation.Cancel()
}

// TestFallbackCostPerTokenIsNotPerMillion guards the unit bug: the manager's
// cost_per_token_usd must produce 10 USD for 1M tokens, not 0.00001 USD.
func TestFallbackCostPerTokenIsNotPerMillion(t *testing.T) {
	mgr := NewWithCost(0, 0, 0.00001, nil)
	estimate, err := mgr.EstimateChat("unknown-model", 1_000_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if estimate.USD != 10 || estimate.Known {
		t.Fatalf("fallback estimate = %+v, want 10 USD unknown", estimate)
	}
}

func TestMonthKeyIsUTC(t *testing.T) {
	plusFive := time.FixedZone("UTC+5", 5*60*60)
	// 2026-02-01 03:00 at UTC+5 is still 2026-01-31 in UTC.
	got := monthKeyAt(time.Date(2026, 2, 1, 3, 0, 0, 0, plusFive))
	if got != "2026-01" {
		t.Fatalf("monthKeyAt = %q, want 2026-01 (UTC)", got)
	}
	if monthKeyAt(time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC)) != "2026-01" {
		t.Fatal("UTC month boundary changed")
	}
}

func TestQuotaBoundaryIsConsistent(t *testing.T) {
	mgr := New(100, 0, nil)
	if err := mgr.Adjust("tenant", 100, 0); err != nil {
		t.Fatalf("usage exactly at limit must be accepted: %v", err)
	}
	if err := mgr.Check("tenant"); err == nil {
		t.Fatal("Check must reject a fully consumed budget")
	}
	if _, err := mgr.Reserve("tenant", 1, 0); err == nil {
		t.Fatal("Reserve above the limit must be rejected")
	}
	if err := mgr.Adjust("tenant", 1, 0); err == nil {
		t.Fatal("recording usage above the limit must report an error")
	}
}

func TestReservationLeaseReclaimsQuota(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	mgr := NewConfiguredWithStoreOptions(true, 100, 0, 0, nil, Options{
		ReservationTTL: time.Minute,
		Now:            clock,
	})
	reservation, err := mgr.Reserve("tenant", 100, 0)
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if _, err := mgr.Reserve("tenant", 1, 0); err == nil {
		t.Fatal("quota held by an in-flight reservation must not be double-spent")
	}
	// A lost Commit/Cancel must not burn the month: the lease expires.
	now = now.Add(2 * time.Minute)
	if err := mgr.Check("tenant"); err != nil {
		t.Fatalf("expired lease should release quota, got %v", err)
	}
	if _, err := mgr.Reserve("tenant", 100, 0); err != nil {
		t.Fatalf("reserve after lease expiry: %v", err)
	}
	metrics := mgr.MetricsSnapshot()
	if metrics.ExpiredReservations != 1 || metrics.ExpiredTokens != 100 {
		t.Fatalf("metrics = %+v, want one expired 100-token reservation", metrics)
	}
	_ = reservation
}

func TestReconcileReportsExpiredReservations(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	mgr := NewConfiguredWithStoreOptions(true, 100, 0, 0, nil, Options{
		ReservationTTL: time.Minute,
		Now:            clock,
	})
	if _, err := mgr.Reserve("tenant", 40, 0); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	result, err := mgr.Reconcile("tenant")
	if err != nil {
		t.Fatal(err)
	}
	if result.Reservations != 1 || result.Tokens != 40 {
		t.Fatalf("reconcile result = %+v, want one 40-token reservation", result)
	}
	if metrics := mgr.MetricsSnapshot(); metrics.ReconcileRuns != 1 {
		t.Fatalf("ReconcileRuns = %d, want 1", metrics.ReconcileRuns)
	}
}

func TestCommitAfterLeaseExpiryStillRecordsActualUsage(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	mgr := NewConfiguredWithStoreOptions(true, 100, 0, 0, nil, Options{
		ReservationTTL: time.Minute,
		Now:            clock,
	})
	reservation, err := mgr.Reserve("tenant", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := mgr.Reserve("tenant", 90, 0); err != nil {
		t.Fatal(err)
	}
	// The provider finished after the lease expired; the actual cost is still
	// recorded (and reported if it pushes usage over the limit).
	if err := reservation.Commit(10, 0); err != nil {
		t.Fatalf("commit after expired lease = %v", err)
	}
	if err := mgr.Check("tenant"); err == nil {
		t.Fatal("committed usage plus live reservation should exhaust the budget")
	}
}

func TestMemoryPrunesPreviousMonths(t *testing.T) {
	now := time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	mgr := NewConfiguredWithStoreOptions(true, 100, 0, 0, nil, Options{Now: clock})
	if err := mgr.Adjust("tenant", 100, 0); err != nil {
		t.Fatal(err)
	}
	now = time.Date(2026, 2, 1, 0, 1, 0, 0, time.UTC)
	if _, err := mgr.Reserve("tenant", 100, 0); err != nil {
		t.Fatalf("new month should start with a fresh quota: %v", err)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	for key := range mgr.mem {
		if key == memKey("tenant", "2026-01") {
			t.Fatal("previous month entry was not purged")
		}
	}
}

func TestRequireSharedStoreFailsClosed(t *testing.T) {
	store := &toggleStore{}
	mgr := NewConfiguredWithStoreOptions(true, 100, 10, 0, nil, Options{
		Store:              store,
		RequireSharedStore: true,
	})
	if err := mgr.Check("tenant"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Check error = %v, want ErrUnavailable", err)
	}
	if _, err := mgr.Reserve("tenant", 1, 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Reserve error = %v, want ErrUnavailable", err)
	}
	if err := mgr.Adjust("tenant", 1, 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Adjust error = %v, want ErrUnavailable", err)
	}
	if _, err := mgr.Reconcile("tenant"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Reconcile error = %v, want ErrUnavailable", err)
	}

	// The same manager stays fail-closed while the store is active but broken.
	store.setActive(true)
	store.setFail(true) // script execution still fails
	if _, err := mgr.Reserve("tenant", 1, 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Reserve with failing store = %v, want ErrUnavailable", err)
	}
	if got := store.calls.Load(); got == 0 {
		t.Fatal("store was never exercised")
	}
}

func TestRequireSharedStoreOffFallsBackToMemory(t *testing.T) {
	mgr := NewConfiguredWithStoreOptions(true, 100, 0, 0, nil, Options{Store: &toggleStore{}})
	// toggleStore starts inactive; without RequireSharedStore the manager keeps
	// the historical local-accounting fallback.
	reservation, err := mgr.Reserve("tenant", 50, 0)
	if err != nil || reservation == nil {
		t.Fatalf("reserve fallback = %v, %v", reservation, err)
	}
	reservation.Commit(50, 0)
	if err := mgr.Check("tenant"); err != nil {
		t.Fatalf("50/100 should pass: %v", err)
	}
}

func TestCommitWithRetryBoundsAttempts(t *testing.T) {
	store := &toggleStore{}
	store.setActive(true)
	mgr := NewConfiguredWithStoreOptions(true, 100, 10, 0, nil, Options{
		Store:              store,
		RequireSharedStore: true,
	})
	// Construct the reservation directly: reserveRedis would need a live Redis
	// to execute the Lua script, but Commit retry only needs a failing store.
	reservation := &Reservation{
		manager:        mgr,
		tenant:         "tenant",
		month:          monthKey(),
		id:             "reservation-1",
		reservedTokens: 10,
	}
	store.setFail(true)
	store.calls.Store(0)
	if err := reservation.CommitWithRetry(10, 0, 3); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("CommitWithRetry error = %v, want ErrUnavailable", err)
	}
	if got := store.calls.Load(); got != 3 {
		t.Fatalf("commit attempts = %d, want 3", got)
	}
	metrics := mgr.MetricsSnapshot()
	if metrics.FailedCommits != 1 {
		t.Fatalf("FailedCommits = %d, want 1", metrics.FailedCommits)
	}
	// Idempotency: a second Commit does not retry or record twice.
	if err := reservation.Commit(10, 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("second commit = %v, want cached ErrUnavailable", err)
	}
	if got := store.calls.Load(); got != 3 {
		t.Fatalf("second commit retried: calls = %d", got)
	}
}

func TestConcurrentReservationsStayWithinBudget(t *testing.T) {
	mgr := NewConfiguredWithStoreOptions(true, 1000, 0, 0, nil, Options{
		ReservationTTL: time.Hour,
	})
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reservation, err := mgr.Reserve("tenant", 50, 0)
			if err != nil {
				return
			}
			accepted.Add(1)
			if err := reservation.Commit(50, 0); err != nil {
				t.Errorf("commit: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := accepted.Load(); got != 20 {
		t.Fatalf("accepted %d reservations of 50 tokens in a 1000-token budget, want exactly 20", got)
	}
	mgr.mu.Lock()
	u := mgr.mem[memKey("tenant", monthKey())]
	mgr.mu.Unlock()
	if u == nil {
		t.Fatal("expected committed usage")
	}
	if u.tokens != 1000 || u.reservedTokens != 0 {
		t.Fatalf("accounting = %d committed / %d reserved, want 1000/0", u.tokens, u.reservedTokens)
	}
}

// toggleStore is a redisstore.Store whose availability and script execution
// can be toggled. It never talks to a real Redis server.
type toggleStore struct {
	active atomic.Bool
	fail   atomic.Bool
	calls  atomic.Int32
}

func (s *toggleStore) Active() bool { return s.active.Load() }

func (s *toggleStore) WithClient(fn func(*redis.Client) error) error {
	s.calls.Add(1)
	if !s.active.Load() {
		return redisstore.ErrUnavailable
	}
	if s.fail.Load() {
		return errors.New("redis: connection refused")
	}
	return fn(nil)
}

func (s *toggleStore) setActive(v bool) { s.active.Store(v) }
func (s *toggleStore) setFail(v bool)   { s.fail.Store(v) }
