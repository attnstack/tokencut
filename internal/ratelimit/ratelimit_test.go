package ratelimit

import (
	"testing"
	"time"
)

func TestBucketBurst(t *testing.T) {
	b := New(100) // burst = 100
	allowed := 0
	for i := 0; i < 300; i++ {
		if b.Allow() {
			allowed++
		}
	}
	// A tight loop runs in microseconds; refill during it is ~0 tokens at
	// 100 rps, so exactly the burst size gets through.
	if allowed < 100 || allowed > 105 {
		t.Errorf("burst allowed %d, want ~100", allowed)
	}
}

func TestBucketRefill(t *testing.T) {
	b := New(500) // 500 tokens/sec
	for i := 0; i < 500; i++ {
		b.Allow()
	}
	time.Sleep(15 * time.Millisecond) // ~7 tokens at 500 rps
	got := 0
	for i := 0; i < 20; i++ {
		if b.Allow() {
			got++
		}
	}
	if got < 3 {
		t.Errorf("expected refill after 15ms, got %d tokens", got)
	}
}

func TestBucketZeroRateFloor(t *testing.T) {
	b := New(0) // rate floors at 1, burst 1
	if !b.Allow() {
		t.Errorf("first request must pass")
	}
}
