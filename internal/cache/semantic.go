package cache

import (
	"math"
	"strings"
	"time"
	"unicode"
)

// Semantic is a placeholder semantic cache (Fase 7) that does cosine-similarity over
// normalized token bags. It wraps an exact Cache and falls back to it.
// When enabled=false, it behaves as exact cache. Threshold 0.97 by default.
type Semantic struct {
	exact     Cache
	enabled   bool
	threshold float64
}

func NewSemantic(exact Cache, enabled bool, threshold float64) *Semantic {
	if threshold <= 0 {
		threshold = 0.97
	}
	return &Semantic{exact: exact, enabled: enabled, threshold: threshold}
}

func (s *Semantic) Get(key string) ([]byte, bool) {
	return s.exact.Get(key)
}

func (s *Semantic) Set(key string, value []byte, ttl time.Duration) {
	s.exact.Set(key, value, ttl)
}

func (s *Semantic) Delete(key string) {
	s.exact.Delete(key)
}

func (s *Semantic) Stats() Stats {
	return s.exact.Stats()
}

// Similarity computes cosine similarity between two texts (bag-of-words, case-insensitive).
// Used to decide if semantic hit should be considered.
// This is a lightweight placeholder for embedding-based similarity.
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
		} else {
			if cur.Len() > 0 {
				toks = append(toks, cur.String())
				cur.Reset()
			}
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
