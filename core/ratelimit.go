package main

import (
	"sync"
	"time"
)

type tokenBucket struct {
	mu         sync.Mutex
	capacity   float64
	tokens     float64
	fillPerSec float64
	last       time.Time
}

func newTokenBucket(mbps float64) *tokenBucket {
	bytesPerSec := mbps * 1_000_000 / 8
	return &tokenBucket{
		capacity:   bytesPerSec,
		tokens:     bytesPerSec,
		fillPerSec: bytesPerSec,
		last:       time.Now(),
	}
}

func (b *tokenBucket) wait(n int) {
	for {
		b.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(b.last).Seconds()
		b.last = now
		b.tokens += elapsed * b.fillPerSec
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		if b.tokens >= float64(n) {
			b.tokens -= float64(n)
			b.mu.Unlock()
			return
		}
		deficit := float64(n) - b.tokens
		sleepFor := deficit / b.fillPerSec
		b.mu.Unlock()
		if sleepFor > 0.25 {
			sleepFor = 0.25
		}
		time.Sleep(time.Duration(sleepFor * float64(time.Second)))
	}
}
