//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/budget"
	"github.com/fcordero/llm-api-gateway/internal/cache"
	"github.com/fcordero/llm-api-gateway/internal/ratelimit"
	"github.com/fcordero/llm-api-gateway/internal/redisstore"
	"github.com/redis/go-redis/v9"
)

// redisURL returns REDIS_URL or the compose default. Tests are skipped, never
// failed, when no server is reachable so `go test -tags=integration ./...`
// stays usable on a laptop.
func redisURL() string {
	if raw := os.Getenv("REDIS_URL"); raw != "" {
		return raw
	}
	return "redis://localhost:6379/0"
}

func newRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	raw := redisURL()
	opts, err := redis.ParseURL(raw)
	if err != nil {
		t.Skipf("REDIS_URL %q is not a valid redis URL: %v", raw, err)
	}
	// One dial attempt: a missing Redis should skip fast instead of spending
	// go-redis' default retry budget per test.
	opts.MaxRetries = -1
	opts.DialTimeout = time.Second
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Skipf("no Redis at %s: %v", redactRedisURL(raw), err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// redactRedisURL keeps failure messages useful without echoing credentials.
func redactRedisURL(raw string) string {
	if i := strings.LastIndex(raw, "@"); i >= 0 {
		return "redis://***@" + raw[i+1:]
	}
	return raw
}

// uniquePrefix isolates every test's keys. Redis keys are shared by all tests
// in this package, so nothing may assume a pristine database.
func uniquePrefix(t *testing.T) string {
	t.Helper()
	name := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return fmt.Sprintf("itest-%s-%d", name, time.Now().UnixNano())
}

// cleanupKeys deletes matching keys when the test finishes. budget:* keys are
// derived from a hash of the tenant, so they cannot be namespaced with the
// prefix; this suite is not parallel and owns the configured Redis database.
// It still captures a baseline so concurrent unrelated runs keep their keys.
func cleanupKeys(t *testing.T, client *redis.Client, patterns ...string) {
	t.Helper()
	ctx := context.Background()
	baseline := map[string]struct{}{}
	for _, pattern := range patterns {
		for _, key := range scanKeys(t, ctx, client, pattern) {
			baseline[key] = struct{}{}
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, pattern := range patterns {
			var doomed []string
			for _, key := range scanKeys(t, ctx, client, pattern) {
				if _, existed := baseline[key]; !existed {
					doomed = append(doomed, key)
				}
			}
			if len(doomed) > 0 {
				if err := client.Del(ctx, doomed...).Err(); err != nil {
					t.Logf("cleanup %s: %v", pattern, err)
				}
			}
		}
	})
}

func scanKeys(t *testing.T, ctx context.Context, client *redis.Client, pattern string) []string {
	t.Helper()
	var keys []string
	var cursor uint64
	for {
		batch, next, err := client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			t.Logf("scan %s: %v", pattern, err)
			return keys
		}
		keys = append(keys, batch...)
		cursor = next
		if cursor == 0 {
			return keys
		}
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

func TestRedisRateLimitConcurrentBurstUsesAtomicBucket(t *testing.T) {
	client := newRedisClient(t)
	prefix := uniquePrefix(t)
	cleanupKeys(t, client, "ratelimit:"+prefix+"*")

	// 1 RPM keeps refill from adding a token during the race while burst 10
	// defines the exact number of admissions.
	limiter := ratelimit.NewRedisWithStore(redisstore.NewStatic(client), 1, 10)
	t.Cleanup(func() { _ = limiter.Close() })
	key := prefix + ":tenant-a"

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if limiter.AllowWithLimits(key, 1, 1, 10) {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != 10 {
		t.Fatalf("allowed = %d, want exactly 10 (burst)", got)
	}
	if retry := limiter.RetryAfterWithLimits(key, 1, 10); retry <= 0 {
		t.Fatalf("RetryAfter = %v, want > 0", retry)
	}
	if limiter.AllowWithLimits(key, 1, 1, 10) {
		t.Fatal("exhausted bucket admitted another request")
	}
}

func TestRedisRateLimitNewBurstDoesNotReuseOldBucket(t *testing.T) {
	client := newRedisClient(t)
	prefix := uniquePrefix(t)
	cleanupKeys(t, client, "ratelimit:"+prefix+"*")

	limiter := ratelimit.NewRedisWithStore(redisstore.NewStatic(client), 1, 10)
	t.Cleanup(func() { _ = limiter.Close() })
	key := prefix + ":tenant-b"

	for i := 0; i < 10; i++ {
		if !limiter.AllowWithLimits(key, 1, 1, 10) {
			t.Fatalf("request %d of the initial burst was denied", i+1)
		}
	}
	if limiter.AllowWithLimits(key, 1, 1, 10) {
		t.Fatal("burst 10 bucket admitted request 11")
	}
	// limitKey embeds the effective limits: a new burst must get a fresh
	// bucket instead of inheriting the exhausted one.
	if !limiter.AllowWithLimits(key, 1, 1, 20) {
		t.Fatal("fresh burst=20 bucket reused the exhausted burst=10 bucket")
	}
	if limiter.AllowWithLimits(key, 1, 1, 10) {
		t.Fatal("old burst=10 bucket was refilled by the unrelated burst=20 call")
	}
}

func TestRedisRateLimitFallsBackToLocalWhenRedisDies(t *testing.T) {
	raw := redisURL()
	opts, err := redis.ParseURL(raw)
	if err != nil {
		t.Skipf("REDIS_URL %q is invalid: %v", raw, err)
	}
	opts.MaxRetries = -1
	opts.DialTimeout = time.Second
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := client.Ping(ctx).Err(); err != nil {
		cancel()
		_ = client.Close()
		t.Skipf("no Redis at %s: %v", redactRedisURL(raw), err)
	}
	cancel()
	// Closing the client makes every script call fail immediately, which is
	// the same code path as a Redis outage: the local fallback must still
	// limit traffic.
	_ = client.Close()

	limiter := ratelimit.NewRedisWithStore(redisstore.NewStatic(client), 1, 2)
	t.Cleanup(func() { _ = limiter.Close() })
	key := uniquePrefix(t) + ":fallback"

	if !limiter.AllowWithLimits(key, 1, 1, 2) || !limiter.AllowWithLimits(key, 1, 1, 2) {
		t.Fatal("local fallback denied a request inside its burst")
	}
	if limiter.AllowWithLimits(key, 1, 1, 2) {
		t.Fatal("local fallback admitted a request beyond burst=2")
	}
	if retry := limiter.RetryAfterWithLimits(key, 1, 2); retry <= 0 {
		t.Fatalf("degraded RetryAfter = %v, want > 0", retry)
	}
}

func newBudgetManager(t *testing.T, client *redis.Client, tokens int, opts budget.Options) *budget.Manager {
	t.Helper()
	opts.Store = redisstore.NewStatic(client)
	return budget.NewConfiguredWithStoreOptions(true, tokens, 0, 0, nil, opts)
}

func TestRedisBudgetConcurrentReservationsRespectExactQuota(t *testing.T) {
	client := newRedisClient(t)
	prefix := uniquePrefix(t)
	cleanupKeys(t, client, "budget:*")

	mgr := newBudgetManager(t, client, 100, budget.Options{})
	tenant := prefix + ":concurrent"

	var accepted, denied atomic.Int64
	var mu sync.Mutex
	var reservations []*budget.Reservation
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reservation, err := mgr.Reserve(tenant, 20, 0)
			if err != nil {
				if !strings.Contains(err.Error(), "insufficient_quota") {
					t.Errorf("unexpected reserve error: %v", err)
				}
				denied.Add(1)
				return
			}
			accepted.Add(1)
			mu.Lock()
			reservations = append(reservations, reservation)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if got := accepted.Load(); got != 5 {
		t.Fatalf("accepted reservations = %d, want 5 (100/20)", got)
	}
	if got := denied.Load(); got != 5 {
		t.Fatalf("denied reservations = %d, want 5", got)
	}
	for _, reservation := range reservations {
		reservation.Cancel()
	}
}

func TestRedisBudgetCommitAdjustsToActualUsage(t *testing.T) {
	client := newRedisClient(t)
	prefix := uniquePrefix(t)
	cleanupKeys(t, client, "budget:*")

	mgr := newBudgetManager(t, client, 100, budget.Options{})
	tenant := prefix + ":commit"

	reservation, err := mgr.Reserve(tenant, 20, 0)
	if err != nil || reservation == nil {
		t.Fatalf("reserve: %v, %v", reservation, err)
	}
	if err := reservation.Commit(100, 0); err != nil {
		t.Fatalf("commit actual usage: %v", err)
	}
	if err := mgr.Check(tenant); err == nil {
		t.Fatal("Check accepted a fully consumed budget")
	}
	if _, err := mgr.Reserve(tenant, 1, 0); err == nil {
		t.Fatal("Reserve accepted quota above the committed usage")
	}
}

func TestRedisBudgetCancelReleasesQuota(t *testing.T) {
	client := newRedisClient(t)
	prefix := uniquePrefix(t)
	cleanupKeys(t, client, "budget:*")

	mgr := newBudgetManager(t, client, 100, budget.Options{})
	tenant := prefix + ":cancel"

	reservation, err := mgr.Reserve(tenant, 100, 0)
	if err != nil || reservation == nil {
		t.Fatalf("reserve: %v, %v", reservation, err)
	}
	if _, err := mgr.Reserve(tenant, 1, 0); err == nil {
		t.Fatal("second reservation admitted while all quota was reserved")
	}
	reservation.Cancel()

	again, err := mgr.Reserve(tenant, 100, 0)
	if err != nil || again == nil {
		t.Fatalf("reserve after cancel: %v, %v", again, err)
	}
	again.Cancel()
}

func TestRedisBudgetFailsClosedWhenSharedStoreRequired(t *testing.T) {
	mgr := budget.NewConfiguredWithStoreOptions(true, 100, 0, 0, nil, budget.Options{
		Store:              redisstore.New(), // inactive on purpose
		RequireSharedStore: true,
	})

	if _, err := mgr.Reserve("tenant-closed", 10, 0); !errors.Is(err, budget.ErrUnavailable) {
		t.Fatalf("Reserve error = %v, want ErrUnavailable", err)
	}
	if err := mgr.Check("tenant-closed"); !errors.Is(err, budget.ErrUnavailable) {
		t.Fatalf("Check error = %v, want ErrUnavailable", err)
	}
	if err := mgr.Adjust("tenant-closed", 1, 0); !errors.Is(err, budget.ErrUnavailable) {
		t.Fatalf("Adjust error = %v, want ErrUnavailable", err)
	}
	if _, err := mgr.Reconcile("tenant-closed"); !errors.Is(err, budget.ErrUnavailable) {
		t.Fatalf("Reconcile error = %v, want ErrUnavailable", err)
	}
}

func TestRedisBudgetExpiredReservationIsReclaimed(t *testing.T) {
	client := newRedisClient(t)
	prefix := uniquePrefix(t)
	cleanupKeys(t, client, "budget:*")

	var clockMu sync.Mutex
	now := time.Now()
	mgr := newBudgetManager(t, client, 100, budget.Options{
		ReservationTTL: 2 * time.Second,
		Now: func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		},
	})
	tenant := prefix + ":ttl"

	reservation, err := mgr.Reserve(tenant, 100, 0)
	if err != nil || reservation == nil {
		t.Fatalf("reserve: %v, %v", reservation, err)
	}
	if _, err := mgr.Reserve(tenant, 1, 0); err == nil {
		t.Fatal("quota should be fully leased")
	}

	clockMu.Lock()
	now = now.Add(3 * time.Second) // past the 2s reservation TTL
	clockMu.Unlock()

	result, err := mgr.Reconcile(tenant)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.Reservations != 1 || result.Tokens != 100 {
		t.Fatalf("reconcile = %+v, want 1 reservation / 100 tokens", result)
	}
	if metrics := mgr.MetricsSnapshot(); metrics.ExpiredReservations < 1 {
		t.Fatalf("ExpiredReservations = %d, want >= 1", metrics.ExpiredReservations)
	}

	reclaimed, err := mgr.Reserve(tenant, 100, 0)
	if err != nil || reclaimed == nil {
		t.Fatalf("quota not reclaimed after lease expiry: %v, %v", reclaimed, err)
	}
	reclaimed.Cancel()
	reservation.Cancel() // idempotent, already reclaimed
}

func TestRedisCacheSetGetDeleteTTLAndIsolation(t *testing.T) {
	client := newRedisClient(t)
	prefix := uniquePrefix(t)
	cleanupKeys(t, client, "cache:"+prefix+"*")

	c := cache.NewRedisWithStore(redisstore.NewStatic(client))
	key := prefix + ":answer"
	c.Set(key, []byte("payload"), time.Minute)

	got, ok := c.Get(key)
	if !ok || string(got) != "payload" {
		t.Fatalf("Get = %q, %v", got, ok)
	}
	if _, ok := c.Get(prefix + ":other"); ok {
		t.Fatal("cache returned a value for a different key")
	}

	stats := c.Stats()
	if stats.Size == -1 {
		t.Fatal("Stats().Size = -1, want a non-negative count")
	}
	if stats.Size < 1 {
		t.Fatalf("Stats().Size = %d, want at least the key just stored", stats.Size)
	}

	c.Delete(key)
	if _, ok := c.Get(key); ok {
		t.Fatal("Delete left the key readable")
	}

	ttlKey := prefix + ":ttl"
	c.Set(ttlKey, []byte("short"), 100*time.Millisecond)
	if _, ok := c.Get(ttlKey); !ok {
		t.Fatal("TTL key missing before expiry")
	}
	waitFor(t, 2*time.Second, "cache TTL expiry", func() bool {
		_, ok := c.Get(ttlKey)
		return !ok
	})
}

func TestRedisStoreReplaceURLDisableAndFailurePreservesClient(t *testing.T) {
	// A separate client owns cleanup: it must still work after the manager is
	// deliberately disabled at the end of the test.
	admin := newRedisClient(t)
	prefix := uniquePrefix(t)
	key := "itest:redisstore:" + prefix
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = admin.Del(ctx, key).Err()
	})

	mgr := redisstore.New()
	t.Cleanup(func() { _ = mgr.Close() })

	if err := mgr.ReplaceURL(redisURL()); err != nil {
		t.Fatalf("ReplaceURL(valid): %v", err)
	}
	if !mgr.Active() {
		t.Fatal("manager inactive after a successful ReplaceURL")
	}

	ping := func() error {
		return mgr.WithClient(func(client *redis.Client) error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			return client.Ping(ctx).Err()
		})
	}
	if err := ping(); err != nil {
		t.Fatalf("ping through manager: %v", err)
	}
	if err := mgr.WithClient(func(client *redis.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return client.Set(ctx, key, "ok", time.Minute).Err()
	}); err != nil {
		t.Fatalf("set through manager: %v", err)
	}

	// A malformed URL and an unreachable endpoint must both leave the previous
	// client installed (the implementation pings before swapping).
	if err := mgr.ReplaceURL("://bad-url"); err == nil {
		t.Fatal("ReplaceURL accepted a malformed URL")
	}
	if !mgr.Active() {
		t.Fatal("failed URL replacement deactivated the previous client")
	}
	if err := mgr.ReplaceURL("redis://127.0.0.1:1/0"); err == nil {
		t.Fatal("ReplaceURL accepted an unreachable endpoint")
	}
	if !mgr.Active() {
		t.Fatal("unreachable endpoint deactivated the previous client")
	}
	if err := ping(); err != nil {
		t.Fatalf("previous client unusable after failed rotations: %v", err)
	}
	if err := mgr.WithClient(func(client *redis.Client) error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		value, err := client.Get(ctx, key).Result()
		if err != nil {
			return err
		}
		if value != "ok" {
			return fmt.Errorf("value = %q, want ok", value)
		}
		return nil
	}); err != nil {
		t.Fatalf("data written before failed rotations was lost: %v", err)
	}

	if err := mgr.ReplaceURL(""); err != nil {
		t.Fatalf("ReplaceURL(empty): %v", err)
	}
	if mgr.Active() {
		t.Fatal("manager still active after ReplaceURL(\"\")")
	}
	if err := ping(); !errors.Is(err, redisstore.ErrUnavailable) {
		t.Fatalf("WithClient after disable = %v, want ErrUnavailable", err)
	}
}
