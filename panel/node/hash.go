package node

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"strconv"
)

// HashState fingerprints the CONTENT of a desired state.
//
// Hand-rolled rather than json.Marshal + sha256, and the reason is worth stating
// because the shorter version looks obviously better. Go's map marshalling does
// sort keys, but struct field order follows declaration order: inserting a field
// in the middle of NodeInbound would change the encoding of every state, so
// every node's hash would change at once, so every daemon on every node would be
// re-raised at once -- dropping every live connection across the fleet on a
// deploy that changed nothing. Writing the fields out explicitly makes that
// impossible to do by accident: a new field affects the hash only when someone
// adds it here deliberately.
//
// Generation and Hash are excluded. Both describe the delivery rather than the
// configuration; including Generation would make every tick look like a change,
// which is exactly what the hash exists to tell apart.
func HashState(s DesiredState) string {
	h := sha256.New()

	// Sort by id so the master's slice order cannot affect the result. Order
	// carries no meaning here -- a node runs all of them.
	ins := make([]NodeInbound, len(s.Inbounds))
	copy(ins, s.Inbounds)
	sort.Slice(ins, func(i, j int) bool { return ins[i].InboundId < ins[j].InboundId })

	for _, in := range ins {
		writeField(h, strconv.Itoa(in.InboundId))
		writeField(h, in.Tag)
		writeField(h, in.Protocol)
		writeField(h, in.Listen)
		writeField(h, strconv.Itoa(in.Port))
		writeField(h, in.Settings)
		writeField(h, in.StreamSettings)
		writeField(h, in.Sniffing)
		writeField(h, strconv.FormatBool(in.Enable))
		writeField(h, strconv.FormatBool(in.SpeedLimitEnable))
		writeField(h, strconv.FormatBool(in.SpeedLimitSeparate))
		writeField(h, strconv.Itoa(in.SpeedLimitDown))
		writeField(h, strconv.Itoa(in.SpeedLimitUp))
		writeField(h, strconv.FormatInt(in.SpeedLimitAfter, 10))
		writeField(h, strconv.Itoa(in.IPLimit))
		writeField(h, in.IPLimitStrategy)
	}

	// Go randomises map iteration order, so without this sort the same state
	// hashes differently between two calls in one process.
	names := make([]string, 0, len(s.Certs))
	for name := range s.Certs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		writeField(h, name)
		writeField(h, string(s.Certs[name]))
	}

	writeField(h, strconv.Itoa(s.Settings.XrayAPIPort))

	return hex.EncodeToString(h.Sum(nil))
}

// writeField length-prefixes each value so concatenation is unambiguous.
//
// Without the prefix, {"ab","c"} and {"a","bc"} hash identically, so two
// different configurations become indistinguishable and a node keeps serving the
// old one. Cheap insurance against a class of bug that would show up as "the
// node just did not pick up the change" and be nearly impossible to trace.
func writeField(h io.Writer, v string) {
	_, _ = io.WriteString(h, strconv.Itoa(len(v)))
	_, _ = io.WriteString(h, ":")
	_, _ = io.WriteString(h, v)
}
