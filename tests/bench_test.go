package tests

import (
	"testing"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/cache"
	"github.com/fcordero/llm-api-gateway/internal/provider"
	"github.com/fcordero/llm-api-gateway/internal/ratelimit"
)

func BenchmarkLimiter_Allow(b *testing.B) {
	lim := ratelimit.New(60000, 1000)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = lim.Allow("bench-key")
	}
}

func BenchmarkLimiter_Allow_Sharded(b *testing.B) {
	lim := ratelimit.New(60000, 1000)
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = lim.Allow("bench-key-parallel")
		}
	})
}

func BenchmarkLimiter_AllowN_Token(b *testing.B) {
	lim := ratelimit.New(60000, 1000)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = lim.AllowN("bench-tokens", 5)
	}
}

func BenchmarkBuildKey(b *testing.B) {
	req := provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.ChatMessage{{Role: "user", Content: "hello world benchmark test content for hashing"}},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = cache.BuildKey(req)
	}
}

func BenchmarkCache_Get_Hit(b *testing.B) {
	m := cache.NewMemory(1000)
	req := provider.ChatRequest{Model: "gpt-4o", Messages: []provider.ChatMessage{{Role: "user", Content: "hello"}}}
	key := cache.BuildKey(req)
	m.Set(key, []byte(`{"id":"x"}`), 5*time.Second)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = m.Get(key)
	}
}

func BenchmarkCache_Get_Miss(b *testing.B) {
	m := cache.NewMemory(1000)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = m.Get("missing-key-12345")
	}
}
