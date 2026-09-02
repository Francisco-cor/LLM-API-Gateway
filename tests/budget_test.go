package tests

import (
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
