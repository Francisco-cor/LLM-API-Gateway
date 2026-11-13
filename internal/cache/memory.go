package cache

import (
	"container/list"
	"sync"
	"time"
)

// DefaultMaxBytes bounds a memory cache when the caller only configures an
// entry count. Without a byte bound, a handful of large completions can exhaust
// the process heap even though the entry count is small.
const DefaultMaxBytes int64 = 64 << 20

type entry struct {
	key       string
	value     []byte
	expiresAt time.Time // zero means no expiry
	size      int64
}

// Memory is an LRU cache with TTL and dual limits: entry count (maxSize) and
// approximate payload bytes (maxBytes). Byte accounting uses len(key)+len(value).
// Values larger than maxBytes are rejected instead of evicting the whole cache.
type Memory struct {
	mu       sync.Mutex
	maxSize  int
	maxBytes int64
	bytes    int64
	ll       *list.List
	items    map[string]*list.Element
	hits     int64
	misses   int64
	evicted  int64
	rejected int64
}

// NewMemory keeps the historical constructor: maxSize entries plus the default
// 64 MiB byte budget.
func NewMemory(maxSize int) *Memory {
	return NewMemoryWithBytes(maxSize, DefaultMaxBytes)
}

// NewMemoryWithBytes creates a memory cache with both limits. maxBytes <= 0
// uses DefaultMaxBytes; maxSize <= 0 uses 1000 entries.
func NewMemoryWithBytes(maxSize int, maxBytes int64) *Memory {
	if maxSize <= 0 {
		maxSize = 1000
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Memory{
		maxSize:  maxSize,
		maxBytes: maxBytes,
		ll:       list.New(),
		items:    make(map[string]*list.Element),
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
	if !ent.expiresAt.IsZero() && time.Now().After(ent.expiresAt) {
		m.removeElementLocked(el)
		m.misses++
		return nil, false
	}
	m.ll.MoveToFront(el)
	m.hits++
	// Copy to avoid external mutation of the stored value.
	cp := make([]byte, len(ent.value))
	copy(cp, ent.value)
	return cp, true
}

func (m *Memory) Set(key string, value []byte, ttl time.Duration) {
	size := int64(len(key)) + int64(len(value))
	m.mu.Lock()
	defer m.mu.Unlock()
	if size > m.maxBytes {
		// A single oversized value must not evict every useful entry: reject it.
		m.rejected++
		return
	}
	expiresAt := time.Time{}
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}
	if el, ok := m.items[key]; ok {
		ent := el.Value.(*entry)
		m.bytes += size - ent.size
		ent.value = append(ent.value[:0], value...)
		ent.size = size
		ent.expiresAt = expiresAt
		m.ll.MoveToFront(el)
		m.evictLocked()
		return
	}
	ent := &entry{key: key, value: append([]byte(nil), value...), expiresAt: expiresAt, size: size}
	el := m.ll.PushFront(ent)
	m.items[key] = el
	m.bytes += size
	m.evictLocked()
}

// evictLocked enforces both limits, oldest entry first. The just-inserted entry
// is never the only reason the byte limit is exceeded because oversized values
// are rejected before insertion.
func (m *Memory) evictLocked() {
	for m.ll.Len() > m.maxSize || m.bytes > m.maxBytes {
		back := m.ll.Back()
		if back == nil {
			return
		}
		m.removeElementLocked(back)
		m.evicted++
	}
}

func (m *Memory) removeElementLocked(el *list.Element) {
	ent := el.Value.(*entry)
	m.ll.Remove(el)
	if current, ok := m.items[ent.key]; ok && current == el {
		delete(m.items, ent.key)
	}
	m.bytes -= ent.size
	if m.bytes < 0 {
		m.bytes = 0
	}
}

func (m *Memory) Delete(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if el, ok := m.items[key]; ok {
		m.removeElementLocked(el)
	}
}

// PurgeExpired removes expired entries. It is optional: Get and Set also handle
// expiry lazily, but a periodic sweep keeps Stats accurate for idle entries.
func (m *Memory) PurgeExpired() int {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := 0
	for el := m.ll.Back(); el != nil; {
		prev := el.Prev()
		ent := el.Value.(*entry)
		if !ent.expiresAt.IsZero() && now.After(ent.expiresAt) {
			m.removeElementLocked(el)
			removed++
		}
		el = prev
	}
	return removed
}

func (m *Memory) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Stats{
		Hits:      m.hits,
		Misses:    m.misses,
		Size:      m.ll.Len(),
		Bytes:     m.bytes,
		Evictions: m.evicted,
		Rejected:  m.rejected,
	}
}
