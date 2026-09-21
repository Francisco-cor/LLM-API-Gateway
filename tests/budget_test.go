package tests

import (
	"sync"
	"sync/atomic"
	"testing"

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
