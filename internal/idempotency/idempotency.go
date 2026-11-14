// Package idempotency deduplicates client retries with an Idempotency-Key.
//
// A completed 2xx response is stored for a bounded TTL and replayed byte for
// byte when the same key and request body arrive again. Reusing a key with a
// different body is rejected with 409. In-flight duplicates wait for the
// leader instead of calling the provider twice. The store is local by default
// and shared through Redis when one is configured, so replicas agree on
// completed responses (the in-flight lock is also Redis-backed).
//
// Streaming responses are not stored: replaying SSE would break the streaming
// contract, so the middleware bypasses requests with "stream": true.
package idempotency

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/fcordero/llm-api-gateway/internal/cache"
	"github.com/fcordero/llm-api-gateway/internal/redisstore"
)

// Outcome is the result of Begin.
type Outcome int

const (
	// Proceed means the caller owns the key and must Complete or Abort.
	Proceed Outcome = iota
	// Replay means a stored response exists and can be returned directly.
	Replay
	// Conflict means the key was used with a different request body.
	Conflict
	// InProgress means another request owns the key and no response is ready.
	InProgress
)

// Config controls the manager. Zero values are replaced by defaults.
type Config struct {
	Enabled       bool
	TTL           time.Duration
	MaxEntries    int
	MaxBytes      int64
	EntryMaxBytes int64
	WaitTimeout   time.Duration
	LockTTL       time.Duration
}

// Defaults returns the built-in policy for an enabled manager.
func Defaults() Config {
	return Config{
		Enabled:       true,
		TTL:           10 * time.Minute,
		MaxEntries:    10_000,
		MaxBytes:      64 << 20,
		EntryMaxBytes: 8 << 20,
		WaitTimeout:   5 * time.Second,
		LockTTL:       2 * time.Minute,
	}
}

// Entry is the stored response plus the request fingerprint.
type Entry struct {
	Fingerprint string              `json:"fingerprint"`
	Status      int                 `json:"status"`
	Headers     map[string][]string `json:"headers,omitempty"`
	Body        []byte              `json:"body"`
	CreatedAt   time.Time           `json:"created_at"`
}

// Manager coordinates the local store and the optional Redis backend.
type Manager struct {
	mu    sync.RWMutex
	cfg   Config
	local *cache.Memory
	redis redisstore.Store

	// locks are per-process single-flight gates. Hash collisions are
	// impossible in practice (SHA-256); a collision would only serialize two
	// unrelated keys.
	locks sync.Map // string -> *localLock
	now   func() time.Time
}

type localLock struct {
	done chan struct{}
}

// New builds a Manager. redis may be nil (single-process mode).
func New(cfg Config, redis redisstore.Store) *Manager {
	m := &Manager{redis: redis, now: time.Now}
	m.UpdateConfig(cfg)
	return m
}

// UpdateConfig applies policy changes. The local store is rebuilt only when
// its limits change; TTL and lock policy apply to new entries immediately.
func (m *Manager) UpdateConfig(cfg Config) {
	def := Defaults()
	if cfg.TTL <= 0 {
		cfg.TTL = def.TTL
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = def.MaxEntries
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = def.MaxBytes
	}
	if cfg.EntryMaxBytes <= 0 {
		cfg.EntryMaxBytes = def.EntryMaxBytes
	}
	if cfg.WaitTimeout <= 0 {
		cfg.WaitTimeout = def.WaitTimeout
	}
	if cfg.LockTTL <= 0 {
		cfg.LockTTL = def.LockTTL
	}
	m.mu.Lock()
	old := m.cfg
	m.cfg = cfg
	if m.local == nil || old.MaxEntries != cfg.MaxEntries || old.MaxBytes != cfg.MaxBytes {
		m.local = cache.NewMemoryWithBytes(cfg.MaxEntries, cfg.MaxBytes)
	}
	m.mu.Unlock()
}

// Config returns a snapshot.
func (m *Manager) Config() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// Begin reserves key for the request fingerprint, or returns a stored response.
func (m *Manager) Begin(ctx context.Context, key, fingerprint string) (*Reservation, Outcome, Entry) {
	cfg := m.Config()
	if !cfg.Enabled {
		return nil, Proceed, Entry{}
	}
	if entry, ok := m.lookup(ctx, key); ok {
		return nil, outcomeFor(entry, fingerprint), entry
	}

	lock := &localLock{done: make(chan struct{})}
	actual, loaded := m.locks.LoadOrStore(key, lock)
	if loaded {
		return m.waitLocal(ctx, key, actual.(*localLock), fingerprint)
	}

	reservation := &Reservation{manager: m, key: key, fingerprint: fingerprint, local: lock}
	if m.redis != nil {
		token := newToken()
		acquired, err := m.acquireRedisLock(ctx, key, token)
		switch {
		case err != nil:
			// Redis is a shared optimization, not a prerequisite: keep the
			// local reservation so a single replica still deduplicates.
		case !acquired:
			m.releaseLocal(key, lock)
			if entry, ok := m.waitForEntry(ctx, key); ok {
				return nil, outcomeFor(entry, fingerprint), entry
			}
			return nil, InProgress, Entry{}
		default:
			reservation.redisToken = token
		}
	}
	return reservation, Proceed, Entry{}
}

func (m *Manager) waitLocal(ctx context.Context, key string, lock *localLock, fingerprint string) (*Reservation, Outcome, Entry) {
	timer := time.NewTimer(m.Config().WaitTimeout)
	defer timer.Stop()
	select {
	case <-lock.done:
	case <-ctx.Done():
		return nil, InProgress, Entry{}
	case <-timer.C:
		return nil, InProgress, Entry{}
	}
	if entry, ok := m.lookup(ctx, key); ok {
		return nil, outcomeFor(entry, fingerprint), entry
	}
	return nil, InProgress, Entry{}
}

func (m *Manager) waitForEntry(ctx context.Context, key string) (Entry, bool) {
	timer := time.NewTimer(m.Config().WaitTimeout)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return Entry{}, false
		case <-timer.C:
			return Entry{}, false
		case <-ticker.C:
			if entry, ok := m.lookup(ctx, key); ok {
				return entry, true
			}
		}
	}
}

// lookup checks the local store first, then Redis. A Redis hit is copied into
// the local store; Redis failures are treated as a miss.
func (m *Manager) lookup(ctx context.Context, key string) (Entry, bool) {
	m.mu.RLock()
	local := m.local
	ttl := m.cfg.TTL
	m.mu.RUnlock()
	if data, ok := local.Get(key); ok {
		if entry, ok := decodeEntry(data); ok {
			return entry, true
		}
	}
	if m.redis == nil || !m.redis.Active() {
		return Entry{}, false
	}
	data, err := m.redisGet(ctx, key)
	if err != nil || len(data) == 0 {
		return Entry{}, false
	}
	entry, ok := decodeEntry(data)
	if !ok {
		return Entry{}, false
	}
	local.Set(key, data, ttl)
	return entry, true
}

// Complete stores a successful response and releases the reservation.
func (r *Reservation) Complete(ctx context.Context, entry Entry) {
	r.once.Do(func() {
		entry.Fingerprint = r.fingerprint
		entry.CreatedAt = r.manager.now().UTC()
		if data, err := json.Marshal(entry); err == nil {
			cfg := r.manager.Config()
			if int64(len(data)) <= cfg.EntryMaxBytes {
				r.manager.mu.RLock()
				local := r.manager.local
				r.manager.mu.RUnlock()
				local.Set(r.key, data, cfg.TTL)
				r.manager.redisSet(ctx, r.key, data, cfg.TTL)
			}
		}
		r.release(ctx)
	})
}

// Abort releases the reservation without storing anything.
func (r *Reservation) Abort() {
	r.once.Do(func() { r.release(context.Background()) })
}

func (r *Reservation) release(ctx context.Context) {
	if r.redisToken != "" {
		r.manager.releaseRedisLock(ctx, r.key, r.redisToken)
	}
	r.manager.releaseLocal(r.key, r.local)
}

// Reservation is an owned key. Complete or Abort must be called exactly once;
// both are safe to call more than once.
type Reservation struct {
	manager     *Manager
	key         string
	fingerprint string
	local       *localLock
	redisToken  string
	once        sync.Once
}

func (m *Manager) releaseLocal(key string, lock *localLock) {
	if lock == nil {
		return
	}
	if v, ok := m.locks.Load(key); ok && v == lock {
		m.locks.Delete(key)
		close(lock.done)
	}
}

func outcomeFor(entry Entry, fingerprint string) Outcome {
	if entry.Fingerprint != fingerprint {
		return Conflict
	}
	return Replay
}

func decodeEntry(data []byte) (Entry, bool) {
	var entry Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		return Entry{}, false
	}
	return entry, true
}

// Key derives the storage key. The tenant and request body are part of the
// fingerprint passed separately; this function only namespaces the caller key.
func Key(tenant, path, key string) string {
	sum := sha256.Sum256([]byte("idempotency:v1\n" + tenant + "\n" + path + "\n" + key))
	return hex.EncodeToString(sum[:])
}

// Fingerprint hashes the raw request body.
func Fingerprint(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

var errRedisUnavailable = errors.New("redis unavailable")
