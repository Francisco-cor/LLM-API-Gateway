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

// Store serializes access to the active Redis client. Implementations keep the
// client pinned for the duration of WithClient, so ReplaceURL can safely wait
// for in-flight operations before closing the old connection.
type Store interface {
	WithClient(func(*redis.Client) error) error
	Active() bool
}

// Manager owns the gateway's replaceable Redis connection. A failed
// replacement leaves the previous connection untouched.
type Manager struct {
	mu     sync.RWMutex
	client *redis.Client
	url    string
}

func (m *Manager) Active() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.client != nil
}

func New() *Manager {
	return &Manager{}
}

func (m *Manager) WithClient(fn func(*redis.Client) error) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.client == nil {
		return ErrUnavailable
	}
	return fn(m.client)
}

// ReplaceURL pings a new endpoint before swapping it into service. An empty
// URL disables Redis and closes the previous client after active operations
// have completed.
func (m *Manager) ReplaceURL(rawURL string) error {
	if rawURL == "" || rawURL == "${REDIS_URL}" {
		m.mu.Lock()
		old := m.client
		m.client = nil
		m.url = ""
		m.mu.Unlock()
		if old != nil {
			_ = old.Close()
		}
		return nil
	}

	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		return fmt.Errorf("parse redis url: %w", err)
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
	m.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

func (m *Manager) URL() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.url
}

func (m *Manager) Close() error {
	m.mu.Lock()
	old := m.client
	m.client = nil
	m.url = ""
	m.mu.Unlock()
	if old == nil {
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
