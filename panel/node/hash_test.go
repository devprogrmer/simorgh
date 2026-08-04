package node

import "testing"

func state() DesiredState {
	return DesiredState{
		Generation: 7,
		Inbounds: []NodeInbound{
			{InboundId: 2, Tag: "b", Protocol: "wg-c", Port: 51820, Settings: `{"x":1}`},
			{InboundId: 1, Tag: "a", Protocol: "openvpn", Port: 1194, Settings: `{"y":2}`},
		},
		Certs:    map[string][]byte{"ca.crt": []byte("CA"), "a.key": []byte("K")},
		Settings: NodeSettings{XrayAPIPort: 62789},
	}
}

// The hash must not depend on the order the master happened to build the slice
// in, nor on Go's randomised map iteration.
//
// This is correctness, not tidiness: Apply is gated on the hash, and the tick
// runs every few seconds. A state that hashed differently each time would
// re-raise unchanged daemons on every tick, dropping every live connection on
// that node continuously.
func TestHashIsOrderIndependent(t *testing.T) {
	a := state()
	b := state()
	b.Inbounds[0], b.Inbounds[1] = b.Inbounds[1], b.Inbounds[0]
	if HashState(a) != HashState(b) {
		t.Fatalf("reordering inbounds changed the hash: %s vs %s", HashState(a), HashState(b))
	}
}

// Map iteration order is randomised per run, so a single comparison can pass by
// luck. Hash the same state repeatedly and require one answer.
func TestHashIsStableAcrossRuns(t *testing.T) {
	want := HashState(state())
	for i := 0; i < 50; i++ {
		if got := HashState(state()); got != want {
			t.Fatalf("run %d hashed %s, want %s -- map iteration is leaking into the hash", i, got, want)
		}
	}
}

// Generation describes the delivery, not the configuration. Two states differing
// only in it are the same configuration, so they must hash the same -- otherwise
// every tick looks like a change and the gate never holds.
func TestHashIgnoresGeneration(t *testing.T) {
	a := state()
	b := state()
	b.Generation = 99
	if HashState(a) != HashState(b) {
		t.Fatal("generation must not affect the hash")
	}
}

// Hash is the field the hash is stored IN, so letting it feed back in would make
// the value depend on whatever was there before.
func TestHashIgnoresStoredHashField(t *testing.T) {
	a := state()
	b := state()
	b.Hash = "whatever was left over from the previous computation"
	if HashState(a) != HashState(b) {
		t.Fatal("the stored Hash field must not feed into the computation")
	}
}

// The other half of the contract: every real change must be visible, or a node
// silently keeps serving the previous configuration.
func TestHashDetectsEveryFieldChange(t *testing.T) {
	base := HashState(state())
	for name, mutate := range map[string]func(*DesiredState){
		"port":        func(s *DesiredState) { s.Inbounds[0].Port = 9999 },
		"settings":    func(s *DesiredState) { s.Inbounds[0].Settings = `{"x":2}` },
		"stream":      func(s *DesiredState) { s.Inbounds[0].StreamSettings = `{"n":"tcp"}` },
		"sniffing":    func(s *DesiredState) { s.Inbounds[0].Sniffing = `{"enabled":true}` },
		"tag":         func(s *DesiredState) { s.Inbounds[0].Tag = "zzz" },
		"protocol":    func(s *DesiredState) { s.Inbounds[0].Protocol = "l2tp" },
		"listen":      func(s *DesiredState) { s.Inbounds[0].Listen = "10.0.0.1" },
		"enable":      func(s *DesiredState) { s.Inbounds[0].Enable = !s.Inbounds[0].Enable },
		"id":          func(s *DesiredState) { s.Inbounds[0].InboundId = 4242 },
		"speedon":     func(s *DesiredState) { s.Inbounds[0].SpeedLimitEnable = true },
		"speedsep":    func(s *DesiredState) { s.Inbounds[0].SpeedLimitSeparate = true },
		"speeddown":   func(s *DesiredState) { s.Inbounds[0].SpeedLimitDown = 512 },
		"speedup":     func(s *DesiredState) { s.Inbounds[0].SpeedLimitUp = 256 },
		"speedafter":  func(s *DesiredState) { s.Inbounds[0].SpeedLimitAfter = 1 << 30 },
		"iplimit":     func(s *DesiredState) { s.Inbounds[0].IPLimit = 3 },
		"ipstrategy":  func(s *DesiredState) { s.Inbounds[0].IPLimitStrategy = "accept" },
		"certchanged": func(s *DesiredState) { s.Certs["ca.crt"] = []byte("OTHER") },
		"certadded":   func(s *DesiredState) { s.Certs["extra.crt"] = []byte("E") },
		"certremoved": func(s *DesiredState) { delete(s.Certs, "ca.crt") },
		"inbounddrop": func(s *DesiredState) { s.Inbounds = s.Inbounds[:1] },
		"nodesetting": func(s *DesiredState) { s.Settings.XrayAPIPort = 1 },
	} {
		s := state()
		mutate(&s)
		if HashState(s) == base {
			t.Errorf("changing %s did not change the hash", name)
		}
	}
}

// Guards the length prefix in writeField. Without it these two states -- which
// are genuinely different configurations -- collide, because their fields
// concatenate to the same bytes.
func TestHashDistinguishesFieldBoundaries(t *testing.T) {
	a := DesiredState{Inbounds: []NodeInbound{{InboundId: 1, Tag: "ab", Protocol: "c"}}}
	b := DesiredState{Inbounds: []NodeInbound{{InboundId: 1, Tag: "a", Protocol: "bc"}}}
	if HashState(a) == HashState(b) {
		t.Fatal("field boundaries are not encoded; concatenation is ambiguous")
	}
}

// The same ambiguity, one level up: a cert named "ab" holding "c" must not hash
// like one named "a" holding "bc".
func TestHashDistinguishesCertBoundaries(t *testing.T) {
	a := DesiredState{Certs: map[string][]byte{"ab": []byte("c")}}
	b := DesiredState{Certs: map[string][]byte{"a": []byte("bc")}}
	if HashState(a) == HashState(b) {
		t.Fatal("cert name/content boundaries are not encoded")
	}
}

// An empty state is a legitimate answer -- a node with every inbound removed --
// and must hash to something stable rather than panicking on the nil map.
func TestHashHandlesEmptyState(t *testing.T) {
	if HashState(DesiredState{}) == "" {
		t.Fatal("an empty state must still produce a hash")
	}
	if HashState(DesiredState{}) != HashState(DesiredState{Inbounds: []NodeInbound{}, Certs: map[string][]byte{}}) {
		t.Fatal("nil and empty collections describe the same state and must hash alike")
	}
}
