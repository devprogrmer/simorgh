package main

import (
	"sync"
	"time"
)

// linkStats tracks live RTT and loss via an explicit ping/pong exchange -
// this works even when the link is otherwise idle, unlike linkQuality
// (which only sees gaps in real data traffic).
type linkStats struct {
	mu      sync.Mutex
	pending map[uint64]time.Time
	rttEWMA float64
	haveRTT bool
	sent    int
	acked   int
}

func newLinkStats() *linkStats {
	return &linkStats{pending: make(map[uint64]time.Time)}
}

func (l *linkStats) recordSend(token uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending[token] = time.Now()
	l.sent++
	cutoff := time.Now().Add(-5 * time.Second)
	for k, t := range l.pending {
		if t.Before(cutoff) {
			delete(l.pending, k)
		}
	}
}

func (l *linkStats) recordPong(token uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sentAt, ok := l.pending[token]
	if !ok {
		return
	}
	delete(l.pending, token)
	rtt := time.Since(sentAt).Seconds() * 1000
	if !l.haveRTT {
		l.rttEWMA = rtt
		l.haveRTT = true
	} else {
		l.rttEWMA = l.rttEWMA*0.8 + rtt*0.2
	}
	l.acked++
}

// snapshot returns the current smoothed RTT (ms, 0 if not yet known) and the
// loss percentage of pings since the last snapshot, then resets the
// sent/acked counters for the next window.
func (l *linkStats) snapshot() (rttMs, lossPct float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rttMs = l.rttEWMA
	if l.sent == 0 {
		return
	}
	lossPct = 100 * float64(l.sent-l.acked) / float64(l.sent)
	if lossPct < 0 {
		lossPct = 0
	}
	l.sent, l.acked = 0, 0
	return
}

// pathScore combines RTT and loss into one comparable number - lower is
// better. Loss is weighted heavily since it costs a gaming connection far
// more than a few extra milliseconds ever would.
func pathScore(rttMs, lossPct float64) float64 {
	return rttMs + lossPct*20
}
