package main

import (
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// candidate is one configured foreign server the client can reach. Only
// meaningful with more than one server configured; with exactly one, the
// client skips all of this and just talks to it directly.
type candidate struct {
	addr     net.Addr
	hostname string

	consecutiveBetter int     // rounds in a row this candidate has beaten the active path by the switch margin
	lastScore         float64 // lower is better
}

// attemptHandshake performs one X25519 handshake attempt against target and
// waits up to timeout for a valid HelloAck. It's used both for the "real"
// active handshake and for lightweight probes of idle candidate servers -
// probing this way costs one cheap ECDH keypair and gets a genuine
// round-trip measurement over the real path, which is worth more than a
// synthetic ping.
func attemptHandshake(tr transport, sessID uint16, password string, target net.Addr, pendingMap *sync.Map, seqCounter *uint32, timeout time.Duration) (*session, time.Duration, error) {
	msg, priv, err := buildHello(password)
	if err != nil {
		return nil, 0, fmt.Errorf("build hello: %w", err)
	}
	clientPub := priv.PublicKey().Bytes()
	resultCh := make(chan *session, 1)
	key := target.String()
	pendingMap.Store(key, &pendingHandshake{priv: priv, clientPub: clientPub, resultCh: resultCh})
	defer pendingMap.Delete(key)

	start := time.Now()
	envelope := buildEnvelope(sessID, nextSeq(seqCounter), msg)
	if err := tr.Send(target, envelope); err != nil {
		return nil, 0, fmt.Errorf("send hello: %w", err)
	}

	select {
	case sess := <-resultCh:
		return sess, time.Since(start), nil
	case <-time.After(timeout):
		return nil, 0, fmt.Errorf("timeout waiting for hello-ack")
	}
}

// completeHandshake resolves a pending handshake (active or probe) when its
// HelloAck arrives. It never touches "current" - callers decide separately
// whether to actually activate the resulting session.
func completeHandshake(pendingMap *sync.Map, password string, from net.Addr, body []byte) {
	v, ok := pendingMap.Load(from.String())
	if !ok {
		return
	}
	p := v.(*pendingHandshake)

	serverPub, err := parseHelloAck(password, p.clientPub, body[1:])
	if err != nil {
		log.Printf("[warn] handshake: %v", err)
		return
	}
	shared, err := p.priv.ECDH(serverPub)
	if err != nil {
		log.Printf("[warn] ecdh: %v", err)
		return
	}
	key := deriveSessionKey(shared, p.clientPub, serverPub.Bytes())
	sc, err := newSessionCrypto(key)
	if err != nil {
		log.Printf("[warn] session crypto: %v", err)
		return
	}
	sess := &session{crypt: sc, peer: from}
	pendingMap.Delete(from.String())
	select {
	case p.resultCh <- sess:
	default:
	}
}

// runHealthChecks probes every candidate except the currently active one,
// scores them against the active path's live RTT/loss, and switches when a
// candidate has clearly and consistently outperformed it. Meant to be
// called once per HEALTH_CHECK_INTERVAL from a ticker.
func runHealthChecks(
	cfg *Config,
	tr transport,
	sessID uint16,
	pendingMap *sync.Map,
	seqCounter *uint32,
	candidates []*candidate,
	activeIdx *atomic.Int32,
	current *atomic.Pointer[session],
	activeStats *linkStats,
) {
	activeRTT, activeLoss := activeStats.snapshot()
	activeScore := pathScore(activeRTT, activeLoss)
	freshLink := activeRTT == 0 // no measurement yet (just started/just switched)

	var wg sync.WaitGroup
	for i, c := range candidates {
		if int32(i) == activeIdx.Load() {
			continue
		}
		wg.Add(1)
		go func(c *candidate) {
			defer wg.Done()
			_, rtt, err := attemptHandshake(tr, sessID, cfg.Password, c.addr, pendingMap, seqCounter, 1500*time.Millisecond)
			if err != nil {
				c.consecutiveBetter = 0
				c.lastScore = pathScore(2000, 100) // treat as a bad, lossy path
				return
			}
			c.lastScore = pathScore(float64(rtt.Milliseconds()), 0)
		}(c)
	}
	wg.Wait()

	bestIdx, bestScore := -1, activeScore
	for i, c := range candidates {
		if int32(i) == activeIdx.Load() {
			continue
		}
		if freshLink || c.lastScore < activeScore-float64(cfg.SwitchMarginMs) {
			if bestIdx == -1 || c.lastScore < bestScore {
				bestIdx, bestScore = i, c.lastScore
			}
		}
	}

	for i, c := range candidates {
		if i == bestIdx {
			c.consecutiveBetter++
		} else if int32(i) != activeIdx.Load() {
			c.consecutiveBetter = 0
		}
	}

	if freshLink || bestIdx == -1 || candidates[bestIdx].consecutiveBetter < cfg.SwitchConfirmRounds {
		return
	}

	// Confirmed: switch for real with a fresh handshake (the probe above is
	// discarded, so this stays fully forward-secret and independent of it).
	target := candidates[bestIdx]
	sess, rtt, err := attemptHandshake(tr, sessID, cfg.Password, target.addr, pendingMap, seqCounter, 1500*time.Millisecond)
	if err != nil {
		log.Printf("[multipath] switch to %s failed: %v", target.hostname, err)
		return
	}
	log.Printf("[multipath] switching active server: %s (score %.1f) -> %s (score %.1f, rtt %v)",
		candidates[activeIdx.Load()].hostname, activeScore, target.hostname, bestScore, rtt)
	current.Store(sess)
	activeIdx.Store(int32(bestIdx))
	target.consecutiveBetter = 0
}
