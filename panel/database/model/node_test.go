package model

import "testing"

// A Node created for the master's own host must be usable with zero remote
// fields set: the local node has no address to dial and no certificate, and
// requiring them would mean either inventing placeholder values or giving the
// local host its own type -- which is exactly the split this model exists to
// avoid.
func TestLocalNodeNeedsNoRemoteFields(t *testing.T) {
	n := Node{Name: LocalNodeName, IsLocal: true, Enable: true}
	if n.Address != "" || n.APIPort != 0 || n.ServerCert != "" {
		t.Fatalf("local node should carry no remote fields, got %+v", n)
	}
	if !n.IsLocal {
		t.Fatal("IsLocal must survive construction")
	}
}

// InboundNode.Port is an override, not a duplicate: 0 means "inherit the
// inbound's own port". Encoding that as a method keeps every call site from
// re-deriving it, because a caller that forgets the rule silently binds port 0
// -- which the kernel reads as "any free port", so the daemon comes up on a
// port nobody can connect to and nothing reports an error.
func TestInboundNodeEffectivePort(t *testing.T) {
	if got := (InboundNode{Port: 0}).EffectivePort(443); got != 443 {
		t.Fatalf("0 should inherit the inbound port, got %d", got)
	}
	if got := (InboundNode{Port: 8443}).EffectivePort(443); got != 8443 {
		t.Fatalf("a set port should override, got %d", got)
	}
}
