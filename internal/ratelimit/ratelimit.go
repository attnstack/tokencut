// ratelimit.go — minimal token-bucket rate limiter (std-lib only).
// Why a limiter on a "transparent" proxy: tokencut protects its upstream
// quota; without it a runaway agent loop burns the user's API budget.
package ratelimit

import (
	"sync"
	"time"
)

type Bucket struct {
	rate   float64 // tokens per second
	burst  float64
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// New returns a bucket allowing rate events/sec with burst = max(rate, 1).
func New(rate float64) *Bucket {
	if rate <= 0 {
		rate = 1
	}
	burst := rate
	if burst < 1 {
		burst = 1
	}
	return &Bucket{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

func (b *Bucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}
