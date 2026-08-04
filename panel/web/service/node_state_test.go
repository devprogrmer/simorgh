package service

import (
	"path/filepath"
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/node"
)

// stateFixture gives a database holding one node and whatever inbounds the test
// places on it, and returns that node's id.
func stateFixture(t *testing.T) (nodeId int) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	var local model.Node
	if err := database.GetDB().Where("is_local = ?", true).First(&local).Error; err != nil {
		t.Fatalf("the seeder should have created a local node: %v", err)
	}
	return local.Id
}

func mustCreate(t *testing.T, v any) {
	t.Helper()
	if err := database.GetDB().Create(v).Error; err != nil {
		t.Fatal(err)
	}
}

// A node is sent the inbounds placed on IT, and nothing else. Sending another
// node's inbounds would have every node serving every inbound -- the same
// clients answered from several hosts, and billed once per host.
func TestBuildDesiredStateOnlyIncludesThisNodesInbounds(t *testing.T) {
	local := stateFixture(t)

	other := model.Node{Name: "frankfurt", Address: "203.0.113.10", APIPort: 62050, Enable: true}
	mustCreate(t, &other)

	mine := model.Inbound{Tag: "mine", Protocol: model.WireGuard, Port: 51820, Enable: true}
	theirs := model.Inbound{Tag: "theirs", Protocol: model.OPENVPN, Port: 1194, Enable: true}
	mustCreate(t, &mine)
	mustCreate(t, &theirs)
	mustCreate(t, &model.InboundNode{InboundId: mine.Id, NodeId: local, Enable: true})
	mustCreate(t, &model.InboundNode{InboundId: theirs.Id, NodeId: other.Id, Enable: true})

	st, err := BuildDesiredState(local)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Inbounds) != 1 {
		t.Fatalf("got %d inbounds; want only the one placed on this node", len(st.Inbounds))
	}
	if st.Inbounds[0].Tag != "mine" {
		t.Fatalf("got inbound %q; want the one placed on this node", st.Inbounds[0].Tag)
	}
}

// The placement's port overrides the inbound's; 0 inherits. Two nodes need not
// have the same port free, which is the reason the override exists at all.
func TestBuildDesiredStateAppliesPortOverride(t *testing.T) {
	local := stateFixture(t)

	inherit := model.Inbound{Tag: "inherit", Protocol: model.WireGuard, Port: 51820, Enable: true}
	override := model.Inbound{Tag: "override", Protocol: model.WireGuard, Port: 51820, Enable: true}
	mustCreate(t, &inherit)
	mustCreate(t, &override)
	mustCreate(t, &model.InboundNode{InboundId: inherit.Id, NodeId: local, Enable: true, Port: 0})
	mustCreate(t, &model.InboundNode{InboundId: override.Id, NodeId: local, Enable: true, Port: 51999})

	st, err := BuildDesiredState(local)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, in := range st.Inbounds {
		got[in.Tag] = in.Port
	}
	if got["inherit"] != 51820 {
		t.Errorf("port 0 should inherit the inbound's 51820, got %d", got["inherit"])
	}
	if got["override"] != 51999 {
		t.Errorf("a set placement port should win, got %d", got["override"])
	}
}

// Disabling either the inbound or its placement must keep it off this node.
// They are different controls -- "this inbound is off everywhere" versus "this
// inbound is off HERE" -- and conflating them would make an operator who
// disabled one location silently disable all of them, or vice versa.
func TestBuildDesiredStateExcludesDisabled(t *testing.T) {
	local := stateFixture(t)

	offInbound := model.Inbound{Tag: "inbound-off", Protocol: model.WireGuard, Port: 1, Enable: false}
	offHere := model.Inbound{Tag: "placement-off", Protocol: model.WireGuard, Port: 2, Enable: true}
	on := model.Inbound{Tag: "on", Protocol: model.WireGuard, Port: 3, Enable: true}
	mustCreate(t, &offInbound)
	mustCreate(t, &offHere)
	mustCreate(t, &on)
	mustCreate(t, &model.InboundNode{InboundId: offInbound.Id, NodeId: local, Enable: true})
	mustCreate(t, &model.InboundNode{InboundId: offHere.Id, NodeId: local, Enable: false})
	mustCreate(t, &model.InboundNode{InboundId: on.Id, NodeId: local, Enable: true})

	st, err := BuildDesiredState(local)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Inbounds) != 1 || st.Inbounds[0].Tag != "on" {
		var tags []string
		for _, in := range st.Inbounds {
			tags = append(tags, in.Tag)
		}
		t.Fatalf("got %v; want only the enabled inbound with an enabled placement", tags)
	}
}

// The hash must describe the state without covering itself, or its value would
// depend on whatever the field happened to hold beforehand.
func TestBuildDesiredStateHashExcludesItself(t *testing.T) {
	local := stateFixture(t)
	in := model.Inbound{Tag: "h", Protocol: model.WireGuard, Port: 51820, Enable: true}
	mustCreate(t, &in)
	mustCreate(t, &model.InboundNode{InboundId: in.Id, NodeId: local, Enable: true})

	st, err := BuildDesiredState(local)
	if err != nil {
		t.Fatal(err)
	}
	if st.Hash == "" {
		t.Fatal("a built state must carry its hash; the node has nothing to report back otherwise")
	}
	bare := st
	bare.Hash = ""
	if want := node.HashState(bare); st.Hash != want {
		t.Fatalf("Hash = %s; want HashState of the state with Hash cleared (%s)", st.Hash, want)
	}
}

// The per-inbound policy columns must reach the node: it is the node that
// enforces them, and a silently dropped limit is an account served without the
// cap its operator set.
func TestBuildDesiredStateCarriesPolicyColumns(t *testing.T) {
	local := stateFixture(t)

	in := model.Inbound{
		Tag: "policy", Protocol: model.WireGuard, Port: 51820, Enable: true,
		Settings: `{"clients":[]}`, StreamSettings: `{"network":"tcp"}`, Sniffing: `{"enabled":true}`,
		SpeedLimitEnable: true, SpeedLimitSeparate: true,
		SpeedLimitDown: 1024, SpeedLimitUp: 512, SpeedLimitAfter: 1 << 30,
		IPLimit: 3, IPLimitStrategy: "accept",
	}
	mustCreate(t, &in)
	mustCreate(t, &model.InboundNode{InboundId: in.Id, NodeId: local, Enable: true})

	st, err := BuildDesiredState(local)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Inbounds) != 1 {
		t.Fatalf("got %d inbounds; want 1", len(st.Inbounds))
	}
	got := st.Inbounds[0]
	for _, c := range []struct {
		name       string
		got, want  any
	}{
		{"Settings", got.Settings, `{"clients":[]}`},
		{"StreamSettings", got.StreamSettings, `{"network":"tcp"}`},
		{"Sniffing", got.Sniffing, `{"enabled":true}`},
		{"SpeedLimitEnable", got.SpeedLimitEnable, true},
		{"SpeedLimitSeparate", got.SpeedLimitSeparate, true},
		{"SpeedLimitDown", got.SpeedLimitDown, 1024},
		{"SpeedLimitUp", got.SpeedLimitUp, 512},
		{"SpeedLimitAfter", got.SpeedLimitAfter, int64(1 << 30)},
		{"IPLimit", got.IPLimit, 3},
		{"IPLimitStrategy", got.IPLimitStrategy, "accept"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v; want %v", c.name, c.got, c.want)
		}
	}
}

// A node with nothing on it is a legitimate state -- a freshly added node, or
// one an operator emptied -- and must build cleanly rather than erroring, since
// the tick would otherwise report every new node as broken.
func TestBuildDesiredStateEmptyNodeIsValid(t *testing.T) {
	local := stateFixture(t)
	st, err := BuildDesiredState(local)
	if err != nil {
		t.Fatalf("an empty node must build: %v", err)
	}
	if len(st.Inbounds) != 0 {
		t.Fatalf("got %d inbounds on an empty node", len(st.Inbounds))
	}
	if st.Hash == "" {
		t.Fatal("even an empty state needs a hash, or the change gate cannot tell it from unset")
	}
}
