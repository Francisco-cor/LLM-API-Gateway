package cache

import (
	"container/list"
	"sync"
	"time"
)

type entry struct {
	key       string
	value     []byte
	expiresAt time.Time
}

// Memory is an LRU cache with TTL (Fase 7, maxSize 1000 by default).
type Memory struct {
	mu      sync.Mutex
	maxSize int
	ll      *list.List
	items   map[string]*list.Element
	hits    int64
	misses  int64
}

func NewMemory(maxSize int) *Memory {
	if maxSize <= 0 {
		maxSize = 1000
	}
	return &Memory{
		maxSize: maxSize,
		ll:      list.New(),
		items:   make(map[string]*list.Element),
	}
}

func (m *Memory) Get(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[key]
	if !ok {
		m.misses++
		return nil, false
	}
	ent := el.Value.(*entry)
	if time.Now().After(ent.expiresAt) {
		m.ll.Remove(el)
		delete(m.items, key)
		m.misses++
		return nil, false
	}
	m.ll.MoveToFront(el)
	m.hits++
	// copy to avoid external mutation
	cp := make([]byte, len(ent.value))
	copy(cp, ent.value)
	return cp, true
}

func (m *Memory) Set(key string, value []byte, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		m.ll.MoveToFront(el)
		ent := el.Value.(*entry)
		ent.value = append([]byte(nil), value...)
		ent.expiresAt = time.Now().Add(ttl)
		return
	}
	if m.ll.Len() >= m.maxSize {
		// evict LRU
		back := m.ll.Back()
		if back != nil {
			m.ll.Remove(back)
			delete(m.items, back.Value.(*entry).key)
		}
	}
	ent := &entry{key: key, value: append([]byte(nil), value...), expiresAt: time.Now().Add(ttl)}
	el := m.ll.PushFront(ent)
	m.items[key] = el
}

func (m *Memory) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		m.ll.Remove(el)
		delete(m.items, key)
	}
}

func (m *Memory) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Stats{Hits: m.hits, Misses: m.misses, Size: m.ll.Len()}
}
