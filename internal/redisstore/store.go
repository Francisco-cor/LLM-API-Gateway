package redisstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrUnavailable means no Redis connection is currently installed.
var ErrUnavailable = errors.New("redis unavailable")

// Store serializes access to the active Redis client. WithClient pins the
// client for the duration of the callback: a concurrent ReplaceURL/Close swaps
// the pointer immediately and closes the retired client only after the last
// in-flight callback returns. Reloads therefore never wait for Redis I/O.
type Store interface {
	WithClient(func(*redis.Client) error) error
	Active() bool
}

// Manager owns the gateway's replaceable Redis connection. A failed
// replacement leaves the previous connection untouched.
//
// Guarantee: the old client stays open while any WithClient callback is using
// it. ReplaceURL never blocks on an in-flight Redis command.
type Manager struct {
	mu      sync.Mutex
	client  *redis.Client
	url     string
	refs    map[*redis.Client]int
	retired map[*redis.Client]struct{}
}

func New() *Manager {
	return &Manager{
		refs:    make(map[*redis.Client]int),
		retired: make(map[*redis.Client]struct{}),
	}
}

func (m *Manager) Active() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.client != nil
}

// WithClient runs fn against the current client without holding the manager
// lock during network I/O.
func (m *Manager) WithClient(fn func(*redis.Client) error) error {
	client, err := m.acquire()
	if err != nil {
		return err
	}
	defer m.release(client)
	return fn(client)
}

func (m *Manager) acquire() (*redis.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client == nil {
		return nil, ErrUnavailable
	}
	if m.refs == nil {
		m.refs = make(map[*redis.Client]int)
	}
	m.refs[m.client]++
	return m.client, nil
}

func (m *Manager) release(client *redis.Client) {
	m.mu.Lock()
	if n := m.refs[client]; n > 1 {
		m.refs[client] = n - 1
		m.mu.Unlock()
		return
	}
	delete(m.refs, client)
	_, closeNow := m.retired[client]
	if closeNow {
		delete(m.retired, client)
	}
	m.mu.Unlock()
	if closeNow {
		_ = client.Close()
	}
}

// retireLocked marks client for closing and reports whether the caller should
// close it now (no in-flight callbacks). The caller must hold m.mu.
func (m *Manager) retireLocked(client *redis.Client) bool {
	if client == nil {
		return false
	}
	if m.retired == nil {
		m.retired = make(map[*redis.Client]struct{})
	}
	if m.refs[client] == 0 {
		delete(m.refs, client)
		return true
	}
	m.retired[client] = struct{}{}
	return false
}

// ReplaceURL pings a new endpoint before swapping it into service. An empty
// URL disables Redis; the previous client is closed after active operations
// have completed. Reload latency is bounded by the ping timeout, not by
// in-flight request traffic.
func (m *Manager) ReplaceURL(rawURL string) error {
	if rawURL == "" || rawURL == "${REDIS_URL}" {
		m.mu.Lock()
		old := m.client
		m.client = nil
		m.url = ""
		closeNow := m.retireLocked(old)
		m.mu.Unlock()
		if closeNow {
			_ = old.Close()
		}
		return nil
	}

	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		return fmt.Errorf("parse redis url: %w", err)
	}
	// The default dial timeout is five seconds; an unreachable Redis during a
	// config reload should not hold the reload for that long.
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 2 * time.Second
	}
	next := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err = next.Ping(ctx).Err()
	cancel()
	if err != nil {
		_ = next.Close()
		return fmt.Errorf("redis ping: %w", err)
	}

	m.mu.Lock()
	if m.url == rawURL && m.client != nil {
		m.mu.Unlock()
		_ = next.Close()
		return nil
	}
	old := m.client
	m.client = next
	m.url = rawURL
	closeNow := m.retireLocked(old)
	m.mu.Unlock()
	if closeNow {
		_ = old.Close()
	}
	return nil
}

func (m *Manager) URL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.url
}

func (m *Manager) Close() error {
	m.mu.Lock()
	old := m.client
	m.client = nil
	m.url = ""
	closeNow := m.retireLocked(old)
	m.mu.Unlock()
	if !closeNow {
		return nil
	}
	return old.Close()
}

// Static adapts an externally owned client for backwards-compatible package
// constructors and tests. The caller remains responsible for closing it.
type Static struct {
	client *redis.Client
}

func (s Static) Active() bool { return s.client != nil }

func NewStatic(client *redis.Client) Store {
	if client == nil {
		return nil
	}
	return Static{client: client}
}

func (s Static) WithClient(fn func(*redis.Client) error) error {
	if s.client == nil {
		return ErrUnavailable
	}
	return fn(s.client)
}
