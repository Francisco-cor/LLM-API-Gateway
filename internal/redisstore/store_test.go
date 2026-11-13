package redisstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestZeroValueManagerIsUsable(t *testing.T) {
	var manager Manager
	if manager.Active() {
		t.Fatal("zero-value manager must be inactive")
	}
	if err := manager.WithClient(func(*redis.Client) error { return nil }); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("WithClient error = %v, want ErrUnavailable", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestReplaceURLDoesNotWaitForInFlightIO is the regression guard for the old
// implementation, which held RLock for the whole callback and blocked reloads.
func TestReplaceURLDoesNotWaitForInFlightIO(t *testing.T) {
	manager := New()
	client := installTestClient(t, manager)

	started := make(chan struct{})
	release := make(chan struct{})
	callbackDone := make(chan struct{})
	go func() {
		defer close(callbackDone)
		_ = manager.WithClient(func(*redis.Client) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	replaced := make(chan struct{})
	go func() {
		_ = manager.ReplaceURL("")
		close(replaced)
	}()
	select {
	case <-replaced:
	case <-time.After(2 * time.Second):
		t.Fatal("ReplaceURL blocked behind an in-flight WithClient callback")
	}
	if manager.Active() {
		t.Fatal("manager still reports the retired client as active")
	}
	if isClosed(t, client) {
		t.Fatal("retired client was closed while a callback was still using it")
	}

	close(release)
	<-callbackDone
	waitClosed(t, client)
}

func TestDisableClosesIdleClientImmediately(t *testing.T) {
	manager := New()
	client := installTestClient(t, manager)
	if err := manager.ReplaceURL(""); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, client)
}

func TestFailedReplacementKeepsPreviousClient(t *testing.T) {
	manager := New()
	client := installTestClient(t, manager)
	if err := manager.ReplaceURL("://bad-url"); err == nil {
		t.Fatal("invalid URL must fail")
	}
	if !manager.Active() {
		t.Fatal("failed replacement must keep the previous client")
	}
	if isClosed(t, client) {
		t.Fatal("failed replacement must not close the previous client")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, client)
}

func TestWithClientConcurrentReplace(t *testing.T) {
	manager := New()
	client := installTestClient(t, manager)
	var wg sync.WaitGroup
	var unexpected atomic.Int32
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				err := manager.WithClient(func(*redis.Client) error { return nil })
				if err != nil && !errors.Is(err, ErrUnavailable) {
					unexpected.Add(1)
				}
			}
		}()
	}
	replaced := make(chan struct{})
	go func() {
		defer close(replaced)
		time.Sleep(5 * time.Millisecond)
		_ = manager.ReplaceURL("")
	}()
	wg.Wait()
	<-replaced
	if got := unexpected.Load(); got != 0 {
		t.Fatalf("%d unexpected WithClient errors", got)
	}
	waitClosed(t, client)
}

func TestStaticStoreReportsActivity(t *testing.T) {
	if store := NewStatic(nil); store != nil {
		t.Fatal("nil client should not create a static store")
	}
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: time.Millisecond, MaxRetries: -1})
	defer client.Close()
	store := NewStatic(client)
	if !store.Active() {
		t.Fatal("static store with client must be active")
	}
	if err := store.WithClient(func(*redis.Client) error { return nil }); err != nil {
		t.Fatalf("WithClient: %v", err)
	}
}

func installTestClient(t *testing.T, manager *Manager) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 5 * time.Millisecond,
		MaxRetries:  -1,
	})
	manager.mu.Lock()
	manager.client = client
	manager.url = "redis://test-instance"
	manager.mu.Unlock()
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func isClosed(t *testing.T, client *redis.Client) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := client.Ping(ctx).Err()
	return err != nil && strings.Contains(err.Error(), "client is closed")
}

func waitClosed(t *testing.T, client *redis.Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if isClosed(t, client) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("retired redis client was not closed")
}
