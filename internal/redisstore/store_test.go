package redisstore

import (
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestManagerStartsInactiveAndCanBeDisabled(t *testing.T) {
	manager := New()
	if manager.Active() {
		t.Fatal("new manager must not report an active client")
	}
	if err := manager.WithClient(func(_ *redis.Client) error { return nil }); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("WithClient error = %v, want ErrUnavailable", err)
	}
	if err := manager.ReplaceURL(""); err != nil {
		t.Fatalf("disable empty manager: %v", err)
	}
}

func TestStaticStoreReportsActivity(t *testing.T) {
	if store := NewStatic(nil); store != nil {
		t.Fatal("nil client should not create a static store")
	}
}
