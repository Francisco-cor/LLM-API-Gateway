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

// SemanticLookup is the interface the request handler uses. Keep it narrow:
// widening it would force proxy to change in the same release.
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
//
// Namespaces must isolate identity, model and every response-shaping option.
// BuildSemanticNamespace does that; a caller that invents its own namespace
// can leak answers across models. The default threshold (0.97) is deliberately
// conservative: lowering it increases false positives, which are served as if
// they were exact hits.
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

// Lookup embeds the query and returns the best matching cached value. Callers
// that will also store a response should use LookupWithVector so the query is
// embedded once instead of once for the miss and again for SetSemantic.
func (s *Semantic) Lookup(ctx context.Context, namespace, query string) ([]byte, bool) {
	value, _, ok := s.LookupWithVector(ctx, namespace, query)
	return value, ok
}

// LookupWithVector returns the embedding it computed alongside the lookup
// result. On a miss, pass that vector to SetSemanticVector after the provider
// responds. The vector is nil when semantic lookup is disabled or embedding
// failed.
func (s *Semantic) LookupWithVector(ctx context.Context, namespace, query string) ([]byte, []float32, bool) {
	if !s.enabled || s.embed == nil || query == "" {
		return nil, nil, false
	}
	vector, err := s.embed(ctx, query)
	if err != nil || len(vector) == 0 {
		return nil, nil, false
	}
	value, ok := s.LookupVector(ctx, namespace, vector)
	return value, vector, ok
}

// LookupVector resolves a precomputed embedding. It never calls the embedder
// and never holds the index mutex across cache lookups (which may be Redis
// round trips).
func (s *Semantic) LookupVector(_ context.Context, namespace string, vector []float32) ([]byte, bool) {
	if !s.enabled || len(vector) == 0 {
		return nil, false
	}
	now := time.Now()
	s.mu.Lock()
	candidates := make([]semanticCandidate, 0, len(s.entries))
	filtered := s.entries[:0]
	for _, entry := range s.entries {
		if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
			continue
		}
		filtered = append(filtered, entry)
		if entry.namespace != namespace || len(entry.vector) != len(vector) {
			continue
		}
		candidates = append(candidates, semanticCandidate{
			key:       entry.key,
			namespace: entry.namespace,
			vector:    entry.vector,
		})
	}
	s.entries = filtered
	s.mu.Unlock()

	for i := range candidates {
		candidates[i].score = cosine(vector, candidates[i].vector)
	}
	matches := candidates[:0]
	for _, candidate := range candidates {
		if candidate.score >= s.threshold {
			matches = append(matches, candidate)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].score > matches[j].score
	})
	if len(matches) > s.topK {
		matches = matches[:s.topK]
	}
	for _, candidate := range matches {
		if value, ok := s.exact.Get(candidate.key); ok {
			s.mu.Lock()
			s.clock++
			s.touchLocked(candidate.key, candidate.namespace, s.clock)
			s.mu.Unlock()
			return value, true
		}
	}
	return nil, false
}

// SetSemantic stores the exact value and embeds the query for semantic lookup.
// Prefer SetSemanticVector when LookupWithVector already produced the vector.
func (s *Semantic) SetSemantic(ctx context.Context, namespace, query, key string, value []byte, ttl time.Duration) {
	s.exact.Set(key, value, ttl)
	if !s.enabled || s.embed == nil || query == "" {
		return
	}
	vector, err := s.embed(ctx, query)
	if err != nil || len(vector) == 0 {
		return
	}
	s.indexVector(namespace, key, vector, ttl)
}

// SetSemanticVector stores an already-embedded entry. value is written to the
// exact cache, so this API is safe to call directly after a miss. A nil/empty
// vector still stores the exact value.
func (s *Semantic) SetSemanticVector(_ context.Context, namespace, key string, vector []float32, value []byte, ttl time.Duration) {
	s.exact.Set(key, value, ttl)
	if len(vector) == 0 || key == "" {
		return
	}
	s.indexVector(namespace, key, vector, ttl)
}

// indexVector updates the local vector index without touching the exact cache.
func (s *Semantic) indexVector(namespace, key string, vector []float32, ttl time.Duration) {
	if !s.enabled {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock++
	filtered := s.entries[:0]
	for _, entry := range s.entries {
		if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
			continue
		}
		if entry.namespace == namespace && entry.key == key {
			continue
		}
		filtered = append(filtered, entry)
	}
	s.entries = filtered
	s.entries = append(s.entries, semanticEntry{
		namespace: namespace,
		key:       key,
		vector:    append([]float32(nil), vector...),
		expiresAt: expiresAt(now, ttl),
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

func expiresAt(now time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(ttl)
}

type semanticCandidate struct {
	key       string
	namespace string
	vector    []float32
	score     float64
}

func (s *Semantic) touchLocked(key, namespace string, clock uint64) {
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
