package cache

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/provider"
)

func TestMemoryByteLimitEvictsLRU(t *testing.T) {
	m := NewMemoryWithBytes(100, 100)
	m.Set("a", make([]byte, 59), time.Minute) // key+value = 60 bytes
	m.Set("b", make([]byte, 59), time.Minute)
	stats := m.Stats()
	if stats.Size != 1 || stats.Bytes != 60 || stats.Evictions != 1 {
		t.Fatalf("stats = %+v, want size 1, bytes 60, evictions 1", stats)
	}
	if _, ok := m.Get("a"); ok {
		t.Fatal("oldest entry should have been evicted by the byte limit")
	}
	if _, ok := m.Get("b"); !ok {
		t.Fatal("newest entry must remain")
	}
}

func TestMemoryRejectsOversizedValue(t *testing.T) {
	m := NewMemoryWithBytes(10, 100)
	m.Set("keep", []byte("value"), time.Minute)
	m.Set("huge", make([]byte, 200), time.Minute)
	if _, ok := m.Get("huge"); ok {
		t.Fatal("oversized value must not be stored")
	}
	if _, ok := m.Get("keep"); !ok {
		t.Fatal("oversized value must not evict useful entries")
	}
	stats := m.Stats()
	if stats.Rejected != 1 || stats.Size != 1 {
		t.Fatalf("stats = %+v, want one rejection and one live entry", stats)
	}
}

func TestMemoryEntryLimitStillApplies(t *testing.T) {
	m := NewMemoryWithBytes(2, DefaultMaxBytes)
	m.Set("a", []byte("1"), time.Minute)
	m.Set("b", []byte("2"), time.Minute)
	m.Set("c", []byte("3"), time.Minute)
	if _, ok := m.Get("a"); ok {
		t.Error("a should be evicted by maxSize")
	}
	if m.Stats().Size != 2 {
		t.Errorf("size = %d, want 2", m.Stats().Size)
	}
}

func TestMemoryTTLZeroMeansNoExpiry(t *testing.T) {
	m := NewMemoryWithBytes(10, DefaultMaxBytes)
	m.Set("eternal", []byte("v"), 0)
	m.Set("short", []byte("v"), time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if _, ok := m.Get("eternal"); !ok {
		t.Fatal("zero TTL must mean no expiry")
	}
	if _, ok := m.Get("short"); ok {
		t.Fatal("positive TTL must expire")
	}
	if removed := m.PurgeExpired(); removed != 0 {
		t.Fatalf("PurgeExpired removed %d already-lazy-expired entries", removed)
	}
}

func TestMemorySetCopiesValue(t *testing.T) {
	m := NewMemoryWithBytes(10, DefaultMaxBytes)
	value := []byte("original")
	m.Set("k", value, time.Minute)
	value[0] = 'X'
	got, ok := m.Get("k")
	if !ok || string(got) != "original" {
		t.Fatalf("value aliasing: got %q, %v", got, ok)
	}
	got[0] = 'Y'
	again, _ := m.Get("k")
	if string(again) != "original" {
		t.Fatal("Get must return a copy")
	}
}

func TestGroupDoCoalescesConcurrentCalls(t *testing.T) {
	var group Group
	var calls atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	const workers = 50
	var wg sync.WaitGroup
	values := make([]string, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			value, err, _ := group.Do("same-key", func() ([]byte, error) {
				if calls.Add(1) == 1 {
					entered <- struct{}{}
				}
				<-release
				return []byte("payload"), nil
			})
			if err != nil {
				t.Errorf("Do: %v", err)
				return
			}
			values[i] = string(value)
		}(i)
	}
	<-entered
	time.Sleep(50 * time.Millisecond) // let the remaining callers join the flight
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
	for i, value := range values {
		if value != "payload" {
			t.Fatalf("worker %d got %q", i, value)
		}
	}
}

func TestGroupLoadOrStoreCachesAndDeduplicates(t *testing.T) {
	var group Group
	cache := NewMemoryWithBytes(10, DefaultMaxBytes)
	var calls atomic.Int32
	load := func() ([]byte, error) {
		calls.Add(1)
		time.Sleep(10 * time.Millisecond)
		return []byte("value"), nil
	}

	value, hit, err := group.LoadOrStore(cache, "key", time.Minute, load)
	if err != nil || hit || string(value) != "value" {
		t.Fatalf("first load = %q hit=%v err=%v", value, hit, err)
	}

	var wg sync.WaitGroup
	var hits atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, hit, err := group.LoadOrStore(cache, "key", time.Minute, load)
			if err != nil || string(value) != "value" {
				t.Errorf("cached load = %q, %v", value, err)
			}
			if hit {
				hits.Add(1)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("loader calls = %d, want 1", calls.Load())
	}
	if hits.Load() != 20 {
		t.Fatalf("cache hits = %d, want 20", hits.Load())
	}

	// Errors are not cached.
	_, _, err = group.LoadOrStore(cache, "failing", time.Minute, func() ([]byte, error) {
		return nil, context.DeadlineExceeded
	})
	if err == nil {
		t.Fatal("expected loader error")
	}
	if _, ok := cache.Get("failing"); ok {
		t.Fatal("failed loads must not be cached")
	}
}

func TestClampTTL(t *testing.T) {
	tests := []struct {
		name      string
		requested time.Duration
		max       time.Duration
		want      time.Duration
	}{
		{"under cap", time.Minute, time.Hour, time.Minute},
		{"over cap", 2 * time.Hour, time.Hour, time.Hour},
		{"zero requested uses max", 0, time.Hour, time.Hour},
		{"negative requested uses max", -time.Second, time.Hour, time.Hour},
		{"no cap", 2 * time.Hour, 0, 2 * time.Hour},
		{"nothing", 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClampTTL(tt.requested, tt.max); got != tt.want {
				t.Fatalf("ClampTTL(%v, %v) = %v, want %v", tt.requested, tt.max, got, tt.want)
			}
		})
	}
}

func TestBuildIfChanged(t *testing.T) {
	base := Config{Enabled: true, TTL: time.Minute, MaxSize: 100, MaxBytes: 1024}
	if !ConfigEquals(base, base) {
		t.Fatal("identical configs must compare equal")
	}
	if ConfigEquals(base, Config{Enabled: true, TTL: time.Minute, MaxSize: 101, MaxBytes: 1024}) {
		t.Fatal("different max size must compare unequal")
	}

	builds := 0
	build := func(Config) Cache {
		builds++
		return NewMemoryWithBytes(10, DefaultMaxBytes)
	}
	current := NewMemoryWithBytes(10, DefaultMaxBytes)
	if got := BuildIfChanged(current, base, base, build); got != current {
		t.Fatal("unchanged config must reuse the current cache")
	}
	if builds != 0 {
		t.Fatalf("rebuilt %d times for unchanged config", builds)
	}
	next := base
	next.TTL = 2 * time.Minute
	if got := BuildIfChanged(current, base, next, build); got == current {
		t.Fatal("changed config must build a new cache")
	}
	if builds != 1 {
		t.Fatalf("builds = %d, want 1", builds)
	}
	// A nil current cache always builds so startup enable works.
	_ = BuildIfChanged(nil, base, base, build)
	if builds != 2 {
		t.Fatalf("nil cache should rebuild, builds = %d", builds)
	}
}

func TestSemanticLookupWithVectorAvoidsSecondEmbedding(t *testing.T) {
	var embeds atomic.Int32
	embed := func(_ context.Context, text string) ([]float32, error) {
		embeds.Add(1)
		if strings.Contains(text, "weather") {
			return []float32{1, 0}, nil
		}
		return []float32{0, 1}, nil
	}
	sem := NewSemanticWithEmbedder(NewMemoryWithBytes(10, DefaultMaxBytes), true, 0.95, 2, 10, embed)
	value, vector, ok := sem.LookupWithVector(context.Background(), "ns", "what is the weather")
	if ok || value != nil || vector == nil {
		t.Fatalf("first lookup = %q %v %v, want miss with vector", value, vector, ok)
	}
	if embeds.Load() != 1 {
		t.Fatalf("embeds = %d, want 1", embeds.Load())
	}
	// The response arrives; reuse the vector instead of embedding the query
	// again in SetSemantic.
	sem.SetSemanticVector(context.Background(), "ns", "key", vector, []byte("sunny"), time.Minute)
	if embeds.Load() != 1 {
		t.Fatalf("SetSemanticVector embedded again: %d calls", embeds.Load())
	}
	value, ok = sem.Lookup(context.Background(), "ns", "weather forecast")
	if !ok || string(value) != "sunny" {
		t.Fatalf("semantic hit = %q, %v", value, ok)
	}
	if _, ok := sem.Lookup(context.Background(), "other-namespace", "weather forecast"); ok {
		t.Fatal("semantic entries must be isolated by namespace")
	}
}

func TestSemanticVectorsExpire(t *testing.T) {
	embed := func(context.Context, string) ([]float32, error) { return []float32{1, 0}, nil }
	sem := NewSemanticWithEmbedder(NewMemoryWithBytes(10, DefaultMaxBytes), true, 0.9, 1, 10, embed)
	sem.SetSemanticVector(context.Background(), "ns", "key", []float32{1, 0}, []byte("v"), 5*time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	if _, ok := sem.LookupVector(context.Background(), "ns", []float32{1, 0}); ok {
		t.Fatal("expired semantic vector must not match")
	}
}

func TestSemanticConcurrentLookupsAndStores(t *testing.T) {
	embed := func(_ context.Context, text string) ([]float32, error) {
		if strings.Contains(text, "a") {
			return []float32{1, 0}, nil
		}
		return []float32{0, 1}, nil
	}
	sem := NewSemanticWithEmbedder(NewMemoryWithBytes(1000, DefaultMaxBytes), true, 0.9, 3, 100, embed)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			query := "a"
			if i%2 == 0 {
				query = "b"
			}
			_, _, _ = sem.LookupWithVector(context.Background(), "ns", query)
		}(i)
		go func(i int) {
			defer wg.Done()
			key := "key-" + string(rune('a'+i%26))
			sem.SetSemanticVector(context.Background(), "ns", key, []float32{1, 0}, []byte(key), time.Minute)
		}(i)
	}
	wg.Wait()
	if stats := sem.Stats(); stats.Size == 0 {
		t.Fatal("expected exact entries from SetSemanticVector")
	}
}

func TestBuildSemanticNamespaceIncludesModelShapeAndIdentity(t *testing.T) {
	base := provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.ChatMessage{{Role: "user", Content: "hello"}},
	}
	ns := BuildSemanticNamespace(base, "tenant-a")
	if ns != BuildSemanticNamespace(base, "tenant-a") {
		t.Fatal("namespace must be deterministic")
	}
	if ns == BuildSemanticNamespace(base, "tenant-b") {
		t.Fatal("identity must change the namespace")
	}
	other := base
	other.Model = "claude-sonnet-4-6"
	if ns == BuildSemanticNamespace(other, "tenant-a") {
		t.Fatal("model must change the namespace")
	}
	temperature := 0.2
	other = base
	other.Temperature = &temperature
	if ns == BuildSemanticNamespace(other, "tenant-a") {
		t.Fatal("sampling options must change the namespace")
	}
	// Message content is deliberately excluded: semantic matching compares it.
	other = base
	other.Messages = []provider.ChatMessage{{Role: "user", Content: "different"}}
	if ns != BuildSemanticNamespace(other, "tenant-a") {
		t.Fatal("message content must not be part of the semantic namespace")
	}
}

func TestRedisCacheStatsNeverPublishUnknownSize(t *testing.T) {
	r := NewRedisWithStore(nil)
	if stats := r.Stats(); stats.Size != 0 || stats.Hits != 0 || stats.Misses != 0 {
		t.Fatalf("empty stats = %+v, want zeros (not -1)", stats)
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("nil store must miss")
	}
	stats := r.Stats()
	if stats.Misses != 1 || stats.Hits != 0 || stats.Size != 0 {
		t.Fatalf("stats after miss = %+v", stats)
	}
}
