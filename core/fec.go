package main

import (
	"encoding/binary"
	"sync"
	"time"
)

// fecSlotSize bounds every frame protected by FEC. It must comfortably cover
// the configured MTU plus TUN framing overhead; 2048 covers any MTU up to
// ~2000 with margin.
const fecSlotSize = 2048

// fecEncoder groups outgoing frames and, once a group is full, produces one
// XOR parity packet covering that whole group. Data packets are always sent
// immediately as they arrive - FEC only adds a trailing parity packet, it
// never delays real data, so it costs no extra round-trip latency; it only
// spends a little bandwidth to let the far end recover a single lost packet
// per group without waiting for a retransmit.
type fecEncoder struct {
	mu        sync.Mutex
	groupSize int
	groupID   uint32
	index     int
	accum     [fecSlotSize]byte
}

func newFECEncoder(groupSize int) *fecEncoder {
	if groupSize < 2 {
		groupSize = 2
	}
	if groupSize > 64 {
		groupSize = 64
	}
	return &fecEncoder{groupSize: groupSize, groupID: 1}
}

// GroupSize returns the configured group size (immutable after construction,
// safe to read without locking).
func (f *fecEncoder) GroupSize() int { return f.groupSize }

// SetGroupSize adjusts the group size for subsequent groups. It only takes
// effect between groups (index == 0) so an in-progress group's accounting
// is never disturbed; call it as often as you like, e.g. once per keepalive
// tick, and it applies whenever a safe moment comes up.
func (f *fecEncoder) SetGroupSize(n int) {
	if n < 2 {
		n = 2
	}
	if n > 64 {
		n = 64
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.index == 0 {
		f.groupSize = n
	}
}

// adaptiveGroupSize maps a peer-reported receive quality percentage to a
// FEC group size: a noisier link gets more frequent (cheaper, smaller)
// parity packets for faster recovery, a clean link gets less overhead.
// peerQualityPct <= 0 means "unknown yet" - keep whatever's configured.
func adaptiveGroupSize(configured, peerQualityPct int) int {
	switch {
	case peerQualityPct <= 0:
		return configured
	case peerQualityPct < 80:
		return 4
	case peerQualityPct < 95:
		return 8
	default:
		return 16
	}
}

// add folds frame into the current group and returns the (groupID, index)
// pair to stamp on the outgoing data packet, plus whether the group just
// completed (in which case the caller should also call flushParity).
func (f *fecEncoder) add(frame []byte) (groupID uint32, index int, complete bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	groupID = f.groupID
	index = f.index

	if len(frame) <= fecSlotSize-2 {
		var slot [fecSlotSize]byte
		binary.BigEndian.PutUint16(slot[0:2], uint16(len(frame)))
		copy(slot[2:], frame)
		for i := range f.accum {
			f.accum[i] ^= slot[i]
		}
	}

	f.index++
	complete = f.index >= f.groupSize
	if complete {
		f.index = 0
	}
	return
}

// flushParity returns the parity packet for the group just completed and
// advances to a new group.
func (f *fecEncoder) flushParity() (groupID uint32, groupSize int, parity []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	groupID = f.groupID
	groupSize = f.groupSize
	parity = append([]byte(nil), f.accum[:]...)
	for i := range f.accum {
		f.accum[i] = 0
	}
	f.groupID++
	return
}

type fecGroupState struct {
	mask     uint64
	accum    [fecSlotSize]byte
	size     int
	seen     int
	lastSeen time.Time
}

// fecDecoder tracks in-flight groups on the receive side and reconstructs
// exactly one missing slot per group when its parity packet allows it.
type fecDecoder struct {
	mu     sync.Mutex
	groups map[uint32]*fecGroupState
}

func newFECDecoder() *fecDecoder {
	return &fecDecoder{groups: make(map[uint32]*fecGroupState)}
}

func (d *fecDecoder) group(groupID uint32, groupSize int) *fecGroupState {
	g, ok := d.groups[groupID]
	if !ok {
		g = &fecGroupState{size: groupSize}
		d.groups[groupID] = g
	}
	return g
}

// onData folds a received data slot into its group's running accumulator so
// a later parity packet can potentially recover a sibling that was lost.
func (d *fecDecoder) onData(groupID uint32, index, groupSize int, frame []byte) {
	if groupSize <= 0 || index < 0 || index >= 64 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	g := d.group(groupID, groupSize)

	bit := uint64(1) << uint(index)
	if g.mask&bit != 0 {
		return // duplicate, already folded in
	}

	if len(frame) <= fecSlotSize-2 {
		var slot [fecSlotSize]byte
		binary.BigEndian.PutUint16(slot[0:2], uint16(len(frame)))
		copy(slot[2:], frame)
		for i := range g.accum {
			g.accum[i] ^= slot[i]
		}
	}
	g.mask |= bit
	g.seen++
	g.lastSeen = time.Now()
}

// onParity attempts to recover exactly one missing slot from the group.
// Returns the recovered frame, or nil if the group is already complete or
// too many slots are missing to recover.
func (d *fecDecoder) onParity(groupID uint32, groupSize int, parity []byte) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()

	g, ok := d.groups[groupID]
	delete(d.groups, groupID) // this group is resolved one way or another
	if !ok || g.size != groupSize || len(parity) != fecSlotSize {
		return nil
	}
	missing := groupSize - g.seen
	if missing != 1 {
		return nil
	}

	var recovered [fecSlotSize]byte
	for i := range recovered {
		recovered[i] = g.accum[i] ^ parity[i]
	}
	n := binary.BigEndian.Uint16(recovered[0:2])
	if int(n) > fecSlotSize-2 {
		return nil
	}
	return append([]byte(nil), recovered[2:2+n]...)
}

// gc drops groups that never completed (parity lost too, or too many drops)
// so the map doesn't grow unbounded.
func (d *fecDecoder) gc(maxAge time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for id, g := range d.groups {
		if now.Sub(g.lastSeen) > maxAge {
			delete(d.groups, id)
		}
	}
}
