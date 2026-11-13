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
	halfOpenAt   time.Time
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

// Allow reports whether a request should proceed. Every true result consumes a
// half-open permission that the caller must settle with RecordSuccess,
// RecordFailure, Release or RecordCancel.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	b.maybeHalfOpenLocked(now)

	switch b.state {
	case StateOpen:
		return false
	case StateHalfOpen:
		if b.halfOpenPend >= b.cfg.HalfOpenMax {
			// A permission was granted but never recorded (caller crashed,
			// request was abandoned, or a bug skipped the settle path). Without
			// this deadline a single leaked probe would wedge the breaker in
			// half-open forever and the provider would never recover.
			if b.halfOpenAt.IsZero() || now.Sub(b.halfOpenAt) < b.cfg.OpenTimeout {
				return false
			}
			b.halfOpenPend = 0
			b.successes = 0
			b.halfOpenAt = now
		}
		if b.halfOpenPend == 0 {
			b.halfOpenAt = now
		}
		b.halfOpenPend++
		return true
	default:
		return true
	}
}

// maybeHalfOpenLocked transitions open -> half-open once the timeout elapsed.
func (b *Breaker) maybeHalfOpenLocked(now time.Time) {
	if b.state != StateOpen {
		return
	}
	if now.Sub(b.lastOpenedAt) < b.cfg.OpenTimeout {
		return
	}
	b.state = StateHalfOpen
	b.halfOpenPend = 0
	b.successes = 0
	b.halfOpenAt = now
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
		b.releaseHalfOpenLocked()
		if b.successes >= b.cfg.HalfOpenMax {
			b.state = StateClosed
			b.failures = 0
			b.successes = 0
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
		b.releaseHalfOpenLocked()
		b.successes = 0
	}
}

// Release settles a half-open permission without recording success or failure.
// Callers use it when a request was cancelled or failed with a non-retryable
// client error: neither says anything about provider health.
func (b *Breaker) Release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateHalfOpen {
		b.releaseHalfOpenLocked()
	}
}

// RecordCancel is the explicit cancellation flavour of Release.
func (b *Breaker) RecordCancel() { b.Release() }

func (b *Breaker) releaseHalfOpenLocked() {
	b.halfOpenPend--
	if b.halfOpenPend < 0 {
		b.halfOpenPend = 0
	}
}

// State returns current state (for metrics/health). It is a read-only snapshot:
// it never opens half-open or mutates pending counters, so calling it in a
// metrics scrape cannot consume a probe permission.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == StateOpen && time.Since(b.lastOpenedAt) >= b.cfg.OpenTimeout {
		return StateHalfOpen
	}
	return b.state
}

// StateReadOnly is an alias of State kept so call sites read as pure
// observability. State itself is already non-mutating.
func (b *Breaker) StateReadOnly() State { return b.State() }

// Reset forces closed (for tests).
func (b *Breaker) Reset() {
	b.mu.Lock()
	b.state = StateClosed
	b.failures = 0
	b.successes = 0
	b.halfOpenPend = 0
	b.halfOpenAt = time.Time{}
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
