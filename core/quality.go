package main

import (
	"log"
	"sync"
)

// linkQuality estimates receive-side packet loss over a rolling window by
// watching gaps in the peer's sequence numbers. It never needs a round
// trip, so it stays cheap enough to check on every packet.
type linkQuality struct {
	mu         sync.Mutex
	haveLast   bool
	lastSeq    uint16
	received   int
	expected   int
	warnPct    int
	banPct     int
	windowPkts int
	lastPct    int
}

func newLinkQuality(warnPct, banPct int) *linkQuality {
	return &linkQuality{warnPct: warnPct, banPct: banPct, windowPkts: 200, lastPct: 100}
}

// Current returns the most recently computed quality percentage (0-100)
// without requiring a new sample - used by adaptive FEC and failover logic.
func (q *linkQuality) Current() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lastPct
}

func (q *linkQuality) observe(seq uint16) (qualityPct int, banned bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.haveLast {
		gap := int(seq - q.lastSeq)
		if gap <= 0 {
			gap = 1
		}
		q.expected += gap
		q.received++
	} else {
		q.expected = 1
		q.received = 1
	}
	q.lastSeq = seq
	q.haveLast = true

	if q.expected > q.windowPkts {
		ratio := float64(q.received) / float64(q.expected)
		q.expected = q.windowPkts
		q.received = int(ratio * float64(q.windowPkts))
	}

	if q.expected == 0 {
		return 100, false
	}
	pct := (q.received * 100) / q.expected
	if pct > 100 {
		pct = 100
	}

	if q.warnPct > 0 && pct < q.warnPct {
		log.Printf("[quality] link quality %d%% is below the warning threshold (%d%%)", pct, q.warnPct)
	}
	if q.banPct > 0 && pct < q.banPct {
		return pct, true
	}
	return pct, false
}
