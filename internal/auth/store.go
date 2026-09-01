package auth

import (
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

// Store holds API keys hashed for constant-time comparison and supports hot-reload.
type Store struct {
	mu   sync.RWMutex
	keys map[string]*Key // hash -> Key
}

// New creates a Store from config keys. Unexpanded env var keys (${...}) are ignored.
func New(keys []config.APIKeyConfig) *Store {
	s := &Store{keys: make(map[string]*Key)}
	s.Reload(keys)
	return s
}

// Reload atomically replaces keys (for SIGHUP / admin reload).
func (s *Store) Reload(keys []config.APIKeyConfig) {
	m := make(map[string]*Key, len(keys))
	for _, k := range keys {
		if k.Key == "" || (len(k.Key) > 3 && k.Key[:2] == "${") {
			continue
		}
		h := hashKey(k.Key)
		m[h] = &Key{
			Tenant:    k.Tenant,
			Scopes:    k.Scopes,
			ExpiresAt: k.ExpiresAt,
			hash:      h,
		}
	}
	s.mu.Lock()
	s.keys = m
	s.mu.Unlock()
}

// Authenticate checks if rawKey is valid, not expired, and returns its Key.
func (s *Store) Authenticate(rawKey string) (*Key, bool) {
	h := hashKey(rawKey)
	s.mu.RLock()
	k, ok := s.keys[h]
	s.mu.RUnlock()
	if !ok {
		// constant-time miss to avoid timing side-channel on existence
		// compare against dummy hash
		dummy := hashKey("dummy")
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

// HasScope reports whether key has scope.
func (k *Key) HasScope(scope string) bool {
	if len(k.Scopes) == 0 {
		return true // no scopes = all allowed
	}
	for _, s := range k.Scopes {
		if s == scope || s == "*" {
			return true
		}
	}
	return false
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
