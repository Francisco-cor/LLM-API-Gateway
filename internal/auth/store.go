package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"sync"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/config"
)

// Key represents a validated API key with metadata.
type Key struct {
	Tenant    string
	Scopes    []string
	ExpiresAt *time.Time
	hash      string
}

// Store holds API keys hashed for constant-time comparison and supports
// hot-reload.
type Store struct {
	mu     sync.RWMutex
	keys   map[string]*Key // hash -> Key
	pepper string
}

// New creates a Store from config keys. Unexpanded env var keys (${...}) are
// ignored.
func New(keys []config.APIKeyConfig) *Store {
	return NewWithPepper(keys, "")
}

// NewWithPepper is New with an optional HMAC-SHA256 pepper. An empty pepper
// keeps the historical plain SHA-256 hash. Changing the pepper invalidates
// existing hashes, which is fine because keys are re-hashed on every reload.
func NewWithPepper(keys []config.APIKeyConfig, pepper string) *Store {
	s := &Store{keys: make(map[string]*Key), pepper: pepper}
	s.Reload(keys)
	return s
}

// Reload atomically replaces keys (for SIGHUP / admin reload), preserving the
// current pepper.
func (s *Store) Reload(keys []config.APIKeyConfig) {
	s.mu.RLock()
	pepper := s.pepper
	s.mu.RUnlock()
	s.ReloadWithPepper(keys, pepper)
}

// ReloadWithPepper swaps both the key set and the pepper in one atomic step so
// a reload can never authenticate against a half-updated hash table.
func (s *Store) ReloadWithPepper(keys []config.APIKeyConfig, pepper string) {
	m := make(map[string]*Key, len(keys))
	for _, k := range keys {
		if k.Key == "" || isPlaceholder(k.Key) {
			continue
		}
		h := hashKey(k.Key, pepper)
		m[h] = &Key{
			Tenant:    k.Tenant,
			Scopes:    append([]string(nil), k.Scopes...),
			ExpiresAt: k.ExpiresAt,
			hash:      h,
		}
	}
	s.mu.Lock()
	s.keys = m
	s.pepper = pepper
	s.mu.Unlock()
}

// Authenticate checks if rawKey is valid, not expired, and returns its Key.
func (s *Store) Authenticate(rawKey string) (*Key, bool) {
	s.mu.RLock()
	pepper := s.pepper
	s.mu.RUnlock()
	h := hashKey(rawKey, pepper)
	s.mu.RLock()
	k, ok := s.keys[h]
	s.mu.RUnlock()
	if !ok {
		// constant-time miss to avoid timing side-channel on existence
		// compare against dummy hash
		dummy := hashKey("dummy", pepper)
		subtle.ConstantTimeCompare([]byte(h), []byte(dummy))
		return nil, false
	}
	if k.ExpiresAt != nil && time.Now().After(*k.ExpiresAt) {
		return nil, false
	}
	// subtle compare hash again (defense in depth)
	if subtle.ConstantTimeCompare([]byte(h), []byte(k.hash)) != 1 {
		return nil, false
	}
	return k, true
}

// HasScope reports whether key has scope. An empty scope list grants nothing;
// full access must be explicit ("*"). auth.enabled with require_scopes (the
// default) rejects empty scope lists at config validation time.
func (k *Key) HasScope(scope string) bool {
	if scope == "" {
		return true
	}
	for _, s := range k.Scopes {
		if s == scope || s == "*" {
			return true
		}
	}
	return false
}

func hashKey(key, pepper string) string {
	if pepper == "" {
		sum := sha256.Sum256([]byte(key))
		return hex.EncodeToString(sum[:])
	}
	mac := hmac.New(sha256.New, []byte(pepper))
	mac.Write([]byte(key))
	return hex.EncodeToString(mac.Sum(nil))
}

func isPlaceholder(v string) bool {
	return len(v) > 3 && v[:2] == "${" && v[len(v)-1] == '}'
}
