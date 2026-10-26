package auth

import (
	"sync"
	"time"
)

// Rate is "N events per Per".
type Rate struct {
	N   int
	Per time.Duration
}

// Limiter counts events per key in a sliding window, in memory. A key holds at most N timestamps, so memory is
// bounded by the number of active keys; Sweep drops idle keys.
type Limiter struct {
	rate Rate
	now  func() time.Time

	mu     sync.Mutex
	events map[string][]time.Time
}

// NewLimiter returns a limiter for rate. A rate with N <= 0 never limits.
func NewLimiter(rate Rate) *Limiter {
	return &Limiter{rate: rate, now: time.Now, events: map[string][]time.Time{}}
}

// prune drops events outside the window; callers hold mu.
func (l *Limiter) prune(key string, now time.Time) []time.Time {
	ev := l.events[key]
	cut := 0
	for cut < len(ev) && now.Sub(ev[cut]) >= l.rate.Per {
		cut++
	}
	if cut > 0 {
		ev = append(ev[:0], ev[cut:]...)
		if len(ev) == 0 {
			delete(l.events, key)
			return nil
		}
		l.events[key] = ev
	}
	return ev
}

// Blocked reports whether key already used up its budget, without recording anything.
func (l *Limiter) Blocked(key string) bool {
	if l.rate.N <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key, l.now())) >= l.rate.N
}

// Add records one event for key.
func (l *Limiter) Add(key string) {
	if l.rate.N <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	ev := l.prune(key, now)
	if len(ev) >= l.rate.N {
		ev = ev[1:] // keep only the newest N; the key stays blocked either way
	}
	l.events[key] = append(ev, now)
}

// Allow records an event and reports whether it was within budget (used where every attempt counts).
func (l *Limiter) Allow(key string) bool {
	if l.rate.N <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	ev := l.prune(key, now)
	if len(ev) >= l.rate.N {
		return false
	}
	l.events[key] = append(ev, now)
	return true
}

// Sweep drops keys whose events all left the window.
func (l *Limiter) Sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for key := range l.events {
		l.prune(key, now)
	}
}
