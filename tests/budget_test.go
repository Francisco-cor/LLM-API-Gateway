package tests

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/budget"
)

func TestBudget_CheckAndRecord(t *testing.T) {
	mgr := budget.New(100, 10.0, nil) // 100 tokens, $10
	if err := mgr.Check("tenant-a"); err != nil {
		t.Fatalf("initial check should pass, got %v", err)
	}
	mgr.Record("tenant-a", 50, 5.0)
	if err := mgr.Check("tenant-a"); err != nil {
		t.Fatalf("50 tokens should still pass, got %v", err)
	}
	mgr.Record("tenant-a", 60, 6.0) // total 110 tokens, $11
	if err := mgr.Check("tenant-a"); err == nil {
		t.Fatal("should exceed token budget after 110 tokens")
	}
}

func TestBudget_ReservationsAreAtomic(t *testing.T) {
	mgr := budget.New(100, 0, nil)
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reservation, err := mgr.Reserve("tenant-a", 20, 0)
			if err != nil {
				return
			}
			accepted.Add(1)
			if err := reservation.Commit(20, 0); err != nil {
				// Reaching the exact configured limit is recorded successfully;
				// the next reservation is the one that must be rejected.
				t.Logf("commit reached limit: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := accepted.Load(); got != 5 {
		t.Fatalf("accepted reservations = %d, want 5", got)
	}
}

func TestBudget_ReservationCancelReleasesUsage(t *testing.T) {
	mgr := budget.New(10, 0, nil)
	reservation, err := mgr.Reserve("tenant-a", 10, 0)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	reservation.Cancel()
	if _, err := mgr.Reserve("tenant-a", 10, 0); err != nil {
		t.Fatalf("reservation should be available after cancel: %v", err)
	}
}

func TestBudget_MonthResetIsolation(t *testing.T) {
	mgr := budget.New(10, 0, nil)
	mgr.Record("tenant-b", 10, 0)
	if err := mgr.Check("tenant-b"); err == nil {
		t.Fatal("should exceed after 10 tokens")
	}
	// different tenant should not be affected
	if err := mgr.Check("tenant-c"); err != nil {
		t.Fatalf("tenant-c should not be limited, got %v", err)
	}
}

func TestBudget_Disabled(t *testing.T) {
	mgr := budget.New(0, 0, nil)
	mgr.Record("tenant-x", 1000000, 1000000)
	if err := mgr.Check("tenant-x"); err != nil {
		t.Fatalf("disabled budget should never block, got %v", err)
	}
}

// TestBudget_FallbackUsesPerTokenUnits guards cost_per_token_usd against being
// interpreted as USD per million tokens.
func TestBudget_FallbackUsesPerTokenUnits(t *testing.T) {
	mgr := budget.NewWithCost(0, 0, 0.00001, nil)
	estimate, err := mgr.EstimateChat("unknown-model", 1_000_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if estimate.USD != 10 {
		t.Fatalf("1M fallback tokens = %v USD, want 10", estimate.USD)
	}
}

// TestBudget_AbandonedReservationLease ensures a lost Commit cannot burn the
// monthly quota: the lease expires and the capacity is reusable.
func TestBudget_AbandonedReservationLease(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	mgr := budget.NewConfiguredWithStoreOptions(true, 100, 0, 0, nil, budget.Options{
		ReservationTTL: time.Minute,
		Now:            func() time.Time { return now },
	})
	if _, err := mgr.Reserve("tenant", 100, 0); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := mgr.Reserve("tenant", 100, 0); err != nil {
		t.Fatalf("expired reservation must release quota: %v", err)
	}
	if metrics := mgr.MetricsSnapshot(); metrics.ExpiredReservations == 0 {
		t.Fatal("expired reservation should be observable")
	}
}
