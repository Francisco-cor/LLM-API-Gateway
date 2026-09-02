package resilience

import (
	"sync"
	"time"
)

// State of circuit breaker.
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// CircuitConfig controls breaker thresholds.
type CircuitConfig struct {
	FailureThreshold int           // consecutive failures to open, e.g. 5
	OpenTimeout      time.Duration // how long to stay open, e.g. 30s
	HalfOpenMax      int           // requests allowed in half-open (usually 1)
}

func DefaultCircuitConfig() CircuitConfig {
	return CircuitConfig{
		FailureThreshold: 5,
		OpenTimeout:      30 * time.Second,
		HalfOpenMax:      1,
	}
}

// Breaker implements a per-provider circuit breaker.
type Breaker struct {
	cfg CircuitConfig

	mu           sync.Mutex
	state        State
	failures     int
	successes    int
	lastOpenedAt time.Time
	halfOpenPend int
}

// NewBreaker creates a breaker with cfg.
func NewBreaker(cfg CircuitConfig) *Breaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.OpenTimeout <= 0 {
		cfg.OpenTimeout = 30 * time.Second
	}
	if cfg.HalfOpenMax <= 0 {
		cfg.HalfOpenMax = 1
	}
	return &Breaker{cfg: cfg, state: StateClosed}
}

// Allow reports whether a request should proceed.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		if time.Since(b.lastOpenedAt) >= b.cfg.OpenTimeout {
			b.state = StateHalfOpen
			b.halfOpenPend = 1
			b.successes = 0
			return true
		}
		return false
	case StateHalfOpen:
		if b.halfOpenPend >= b.cfg.HalfOpenMax {
			return false
		}
		b.halfOpenPend++
		return true
	default:
		return true
	}
}

// RecordSuccess records a successful call.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		b.failures = 0
	case StateHalfOpen:
		b.successes++
		if b.successes >= b.cfg.HalfOpenMax {
			b.state = StateClosed
			b.failures = 0
			b.successes = 0
		}
		b.halfOpenPend--
		if b.halfOpenPend < 0 {
			b.halfOpenPend = 0
		}
	case StateOpen:
		// shouldn't happen, but reset if we get success while open (half-open path)
	}
}

// RecordFailure records a failed call (retryable failure).
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		b.failures++
		if b.failures >= b.cfg.FailureThreshold {
			b.state = StateOpen
			b.lastOpenedAt = time.Now()
		}
	case StateHalfOpen:
		b.state = StateOpen
		b.lastOpenedAt = time.Now()
		b.halfOpenPend--
		if b.halfOpenPend < 0 {
			b.halfOpenPend = 0
		}
		b.successes = 0
	}
}

// State returns current state (for metrics/health).
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	// transition open -> half-open lazily on State() as well
	if b.state == StateOpen && time.Since(b.lastOpenedAt) >= b.cfg.OpenTimeout {
		b.state = StateHalfOpen
		b.halfOpenPend = 0
		b.successes = 0
	}
	return b.state
}

// Reset forces closed (for tests).
func (b *Breaker) Reset() {
	b.mu.Lock()
	b.state = StateClosed
	b.failures = 0
	b.successes = 0
	b.halfOpenPend = 0
	b.mu.Unlock()
}

// UpdateConfig updates breaker thresholds atomically (for hot-reload).
func (b *Breaker) UpdateConfig(cfg CircuitConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cfg.FailureThreshold > 0 {
		b.cfg.FailureThreshold = cfg.FailureThreshold
	}
	if cfg.OpenTimeout > 0 {
		b.cfg.OpenTimeout = cfg.OpenTimeout
	}
	if cfg.HalfOpenMax > 0 {
		b.cfg.HalfOpenMax = cfg.HalfOpenMax
	}
}

// Config returns current config (for admin introspection).
func (b *Breaker) Config() CircuitConfig {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg
}
