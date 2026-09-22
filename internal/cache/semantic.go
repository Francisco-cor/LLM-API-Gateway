package cache

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// EmbeddingFunc supplies vectors for semantic cache queries and entries. The
// cache does not choose a provider; the gateway injects the configured
// embedding provider so this component remains independently testable.
type EmbeddingFunc func(context.Context, string) ([]float32, error)

type SemanticLookup interface {
	Lookup(context.Context, string, string) ([]byte, bool)
	SetSemantic(context.Context, string, string, string, []byte, time.Duration)
}

type semanticEntry struct {
	namespace string
	key       string
	vector    []float32
	expiresAt time.Time
	lastUsed  uint64
}

// Semantic is an opt-in vector index over an exact response cache. Exact
// lookup remains available through Cache; semantic lookup is only available
// when an embedding function has been injected. The vector index is local to
// a process, while values can live in memory or Redis through the wrapped
// Cache implementation.
type Semantic struct {
	exact      Cache
	enabled    bool
	threshold  float64
	topK       int
	maxEntries int
	embed      EmbeddingFunc

	mu      sync.Mutex
	entries []semanticEntry
	clock   uint64
}

func NewSemantic(exact Cache, enabled bool, threshold float64) *Semantic {
	return NewSemanticWithEmbedder(exact, enabled, threshold, 3, 1000, nil)
}

func NewSemanticWithEmbedder(exact Cache, enabled bool, threshold float64, topK, maxEntries int, embed EmbeddingFunc) *Semantic {
	if threshold <= 0 {
		threshold = 0.97
	}
	if topK <= 0 {
		topK = 3
	}
	if maxEntries <= 0 {
		maxEntries = 1000
	}
	return &Semantic{
		exact:      exact,
		enabled:    enabled,
		threshold:  threshold,
		topK:       topK,
		maxEntries: maxEntries,
		embed:      embed,
	}
}

func (s *Semantic) Get(key string) ([]byte, bool) {
	return s.exact.Get(key)
}

func (s *Semantic) Set(key string, value []byte, ttl time.Duration) {
	s.exact.Set(key, value, ttl)
}

func (s *Semantic) Delete(key string) {
	s.exact.Delete(key)
	s.mu.Lock()
	filtered := s.entries[:0]
	for _, entry := range s.entries {
		if entry.key != key {
			filtered = append(filtered, entry)
		}
	}
	s.entries = filtered
	s.mu.Unlock()
}

func (s *Semantic) Stats() Stats {
	return s.exact.Stats()
}

func (s *Semantic) Lookup(ctx context.Context, namespace, query string) ([]byte, bool) {
	if !s.enabled || s.embed == nil || query == "" {
		return nil, false
	}
	vector, err := s.embed(ctx, query)
	if err != nil || len(vector) == 0 {
		return nil, false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var candidates []semanticCandidate
	filtered := s.entries[:0]
	for _, entry := range s.entries {
		if now.After(entry.expiresAt) {
			continue
		}
		filtered = append(filtered, entry)
		if entry.namespace != namespace || len(entry.vector) != len(vector) {
			continue
		}
		if score := cosine(vector, entry.vector); score >= s.threshold {
			candidates = append(candidates, semanticCandidate{entry: entry, score: score})
		}
	}
	s.entries = filtered
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	if len(candidates) > s.topK {
		candidates = candidates[:s.topK]
	}
	for _, candidate := range candidates {
		if value, ok := s.exact.Get(candidate.entry.key); ok {
			s.clock++
			s.touch(candidate.entry.key, namespace, s.clock)
			return value, true
		}
	}
	return nil, false
}

func (s *Semantic) SetSemantic(ctx context.Context, namespace, query, key string, value []byte, ttl time.Duration) {
	// Always keep exact caching useful when the optional embedder is absent or
	// temporarily unavailable. Semantic indexing is best effort.
	s.exact.Set(key, value, ttl)
	if !s.enabled || s.embed == nil || query == "" {
		return
	}
	vector, err := s.embed(ctx, query)
	if err != nil || len(vector) == 0 {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock++
	filtered := s.entries[:0]
	for _, entry := range s.entries {
		if now.After(entry.expiresAt) || (entry.namespace == namespace && entry.key == key) {
			continue
		}
		filtered = append(filtered, entry)
	}
	s.entries = filtered
	s.entries = append(s.entries, semanticEntry{
		namespace: namespace,
		key:       key,
		vector:    append([]float32(nil), vector...),
		expiresAt: now.Add(ttl),
		lastUsed:  s.clock,
	})
	for len(s.entries) > s.maxEntries {
		oldest := 0
		for i := 1; i < len(s.entries); i++ {
			if s.entries[i].lastUsed < s.entries[oldest].lastUsed {
				oldest = i
			}
		}
		s.entries = append(s.entries[:oldest], s.entries[oldest+1:]...)
	}
}

type semanticCandidate struct {
	entry semanticEntry
	score float64
}

func (s *Semantic) touch(key, namespace string, clock uint64) {
	for i := range s.entries {
		if s.entries[i].key == key && s.entries[i].namespace == namespace {
			s.entries[i].lastUsed = clock
			return
		}
	}
}

func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// Similarity is retained as a small compatibility helper for callers that
// used the old placeholder. Semantic cache lookups never use this function.
func Similarity(a, b string) float64 {
	tokensA := tokenize(a)
	tokensB := tokenize(b)
	if len(tokensA) == 0 || len(tokensB) == 0 {
		return 0
	}
	freqA := freq(tokensA)
	freqB := freq(tokensB)
	var dot, normA, normB float64
	for k, v := range freqA {
		dot += float64(v) * float64(freqB[k])
		normA += float64(v * v)
	}
	for _, v := range freqB {
		normB += float64(v * v)
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func tokenize(s string) []string {
	s = strings.ToLower(s)
	var toks []string
	var cur strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(r)
		} else if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		toks = append(toks, cur.String())
	}
	return toks
}

func freq(toks []string) map[string]int {
	m := make(map[string]int, len(toks))
	for _, t := range toks {
		m[t]++
	}
	return m
}
