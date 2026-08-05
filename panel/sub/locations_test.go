package sub

import (
	"path/filepath"
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
)

func locDB(t *testing.T) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
}

func mkInbound(t *testing.T, tag string) model.Inbound {
	t.Helper()
	in := model.Inbound{Tag: tag, Protocol: model.VLESS, Port: 443, Enable: true, Remark: tag}
	if err := database.GetDB().Create(&in).Error; err != nil {
		t.Fatal(err)
	}
	return in
}

func mkNode(t *testing.T, name, addr string, enable bool) model.Node {
	t.Helper()
	n := model.Node{Name: name, Address: addr, APIPort: 62050, Enable: enable}
	if err := database.GetDB().Create(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func place(t *testing.T, inboundId, nodeId, port int) {
	t.Helper()
	p := model.InboundNode{InboundId: inboundId, NodeId: nodeId, Enable: true, Port: port}
	if err := database.GetDB().Create(&p).Error; err != nil {
		t.Fatal(err)
	}
}

// The single-location case, which is every install that never adds a node: one
// location, no address (the panel's own host), no name suffix on the remark.
//
// This is the regression that matters most. Multi-location is the same code
// path, not a branch, so if it ever changes what a plain install produces, that
// shows up here rather than in a customer's client app.
func TestSingleLocationIsUnchanged(t *testing.T) {
	locDB(t)
	in := mkInbound(t, "solo")
	// InitDB's seeder already placed it on the local node.

	locs := locationsFor(in.Id)
	if len(locs) != 1 {
		t.Fatalf("got %d locations for a plain install; want exactly 1", len(locs))
	}
	if locs[0].Address != "" {
		t.Errorf("local location should carry no address (it falls back to the panel host), got %q", locs[0].Address)
	}
	if locs[0].Name != "" {
		t.Errorf("local location should carry no name, or a plain install gains a remark suffix nobody asked for; got %q", locs[0].Name)
	}
}

// The feature: one inbound on three nodes is three locations, each with its own
// address and label.
func TestThreeNodesGiveThreeLocations(t *testing.T) {
	locDB(t)
	in := mkInbound(t, "wg")
	// Drop the seeded local placement so this is purely the three remote nodes.
	if err := database.GetDB().Where("inbound_id = ?", in.Id).Delete(&model.InboundNode{}).Error; err != nil {
		t.Fatal(err)
	}
	for _, n := range []struct{ name, addr string }{
		{"Frankfurt", "203.0.113.10"},
		{"Helsinki", "203.0.113.20"},
		{"Amsterdam", "203.0.113.30"},
	} {
		node := mkNode(t, n.name, n.addr, true)
		place(t, in.Id, node.Id, 0)
	}

	locs := locationsFor(in.Id)
	if len(locs) != 3 {
		t.Fatalf("got %d locations; want one per node", len(locs))
	}
	seen := map[string]string{}
	for _, l := range locs {
		seen[l.Name] = l.Address
	}
	for name, want := range map[string]string{
		"Frankfurt": "203.0.113.10",
		"Helsinki":  "203.0.113.20",
		"Amsterdam": "203.0.113.30",
	} {
		if seen[name] != want {
			t.Errorf("%s resolved to %q; want %q", name, seen[name], want)
		}
	}
}

// A placement's port override reaches the generated config. Two nodes need not
// have the same port free, which is why the override exists.
func TestPlacementPortOverrideReachesTheLocation(t *testing.T) {
	locDB(t)
	in := mkInbound(t, "wg")
	database.GetDB().Where("inbound_id = ?", in.Id).Delete(&model.InboundNode{})
	node := mkNode(t, "Frankfurt", "203.0.113.10", true)
	place(t, in.Id, node.Id, 51999)

	locs := locationsFor(in.Id)
	if len(locs) != 1 {
		t.Fatalf("got %d locations", len(locs))
	}
	if locs[0].Port != 51999 {
		t.Errorf("port = %d; want the placement's override", locs[0].Port)
	}
}

// A DISABLED node is left out. Handing a customer a config for a node you have
// switched off makes their client app look broken, and they cannot tell which of
// their three configs is the dead one.
func TestDisabledNodeIsNotOffered(t *testing.T) {
	locDB(t)
	in := mkInbound(t, "wg")
	database.GetDB().Where("inbound_id = ?", in.Id).Delete(&model.InboundNode{})
	live := mkNode(t, "Frankfurt", "203.0.113.10", true)
	off := mkNode(t, "Helsinki", "203.0.113.20", false)
	place(t, in.Id, live.Id, 0)
	place(t, in.Id, off.Id, 0)

	locs := locationsFor(in.Id)
	if len(locs) != 1 {
		t.Fatalf("got %d locations; the disabled node should have been dropped", len(locs))
	}
	if locs[0].Name != "Frankfurt" {
		t.Errorf("kept %q; want the enabled node", locs[0].Name)
	}
}

// A disabled PLACEMENT is also left out -- "serve this inbound everywhere except
// Helsinki" is a different statement from "switch Helsinki off".
func TestDisabledPlacementIsNotOffered(t *testing.T) {
	locDB(t)
	in := mkInbound(t, "wg")
	database.GetDB().Where("inbound_id = ?", in.Id).Delete(&model.InboundNode{})
	a := mkNode(t, "Frankfurt", "203.0.113.10", true)
	b := mkNode(t, "Helsinki", "203.0.113.20", true)
	place(t, in.Id, a.Id, 0)
	if err := database.GetDB().Create(&model.InboundNode{
		InboundId: in.Id, NodeId: b.Id, Enable: false,
	}).Error; err != nil {
		t.Fatal(err)
	}

	locs := locationsFor(in.Id)
	if len(locs) != 1 || locs[0].Name != "Frankfurt" {
		t.Fatalf("got %+v; want only the enabled placement", locs)
	}
}

// An inbound whose placements have all gone must still yield a usable config
// rather than an empty subscription.
//
// Placements are seeded for every inbound, so an empty result means the data is
// wrong. Serving from the panel host keeps a working config in the customer's
// hands while that is investigated; returning nothing silently empties their
// subscription and they find out before you do.
func TestNoPlacementsStillServesSomething(t *testing.T) {
	locDB(t)
	in := mkInbound(t, "orphan")
	database.GetDB().Where("inbound_id = ?", in.Id).Delete(&model.InboundNode{})

	locs := locationsFor(in.Id)
	if len(locs) != 1 {
		t.Fatalf("got %d locations for an unplaced inbound; want a single fallback", len(locs))
	}
	if locs[0].Address != "" || locs[0].Name != "" {
		t.Errorf("the fallback should be the plain local location, got %+v", locs[0])
	}
}

// Same again when every placement points at a node that has been deleted.
func TestPlacementsPointingAtMissingNodesFallBack(t *testing.T) {
	locDB(t)
	in := mkInbound(t, "ghost")
	database.GetDB().Where("inbound_id = ?", in.Id).Delete(&model.InboundNode{})
	place(t, in.Id, 4242, 0) // no such node

	locs := locationsFor(in.Id)
	if len(locs) != 1 || locs[0].Address != "" {
		t.Fatalf("got %+v; want the local fallback", locs)
	}
}

// The relay topology, which is the common one for Iran: the daemon runs on a
// foreign node, but customers must be given the Iranian address that forwards to
// it. Handing them the foreign address instead would bypass the relay entirely
// -- the customer pays for the low-latency first hop and never uses it.
func TestAdvertisedAddressWinsOverTheNodesOwn(t *testing.T) {
	locDB(t)
	in := mkInbound(t, "wg")
	database.GetDB().Where("inbound_id = ?", in.Id).Delete(&model.InboundNode{})
	fra := mkNode(t, "Frankfurt", "203.0.113.10", true)

	if err := database.GetDB().Create(&model.InboundNode{
		InboundId: in.Id, NodeId: fra.Id, Enable: true,
		Listen:    "10.0.0.5",     // where the daemon binds, on the foreign box
		Advertise: "185.51.200.77", // the Iranian relay customers dial
	}).Error; err != nil {
		t.Fatal(err)
	}

	locs := locationsFor(in.Id)
	if len(locs) != 1 {
		t.Fatalf("got %d locations", len(locs))
	}
	if locs[0].Address != "185.51.200.77" {
		t.Fatalf("customers were given %q; want the advertised relay address", locs[0].Address)
	}
	// The label still names the node the daemon runs on, which is what an
	// operator reading the subscription needs to know.
	if locs[0].Name != "Frankfurt" {
		t.Errorf("label = %q; want the node's name", locs[0].Name)
	}
}

// Without an advertised address the node's own is used -- the direct topology,
// and the default.
func TestNodeAddressUsedWhenNothingIsAdvertised(t *testing.T) {
	locDB(t)
	in := mkInbound(t, "wg")
	database.GetDB().Where("inbound_id = ?", in.Id).Delete(&model.InboundNode{})
	fra := mkNode(t, "Frankfurt", "203.0.113.10", true)
	place(t, in.Id, fra.Id, 0)

	locs := locationsFor(in.Id)
	if locs[0].Address != "203.0.113.10" {
		t.Fatalf("address = %q; want the node's own", locs[0].Address)
	}
}

// An OFFLINE node still ships its configs. Offline means the panel has not
// reached its control channel for three ticks, which is very often the
// management path rather than the data path -- the VPN daemons keep serving
// whatever they last had. Withholding those configs turns a management blip into
// an outage for customers who are connected and perfectly fine.
func TestOfflineNodeStillShipsConfigs(t *testing.T) {
	locDB(t)
	in := mkInbound(t, "wg")
	database.GetDB().Where("inbound_id = ?", in.Id).Delete(&model.InboundNode{})
	n := mkNode(t, "Frankfurt", "203.0.113.10", true)
	database.GetDB().Model(&n).Update("status", model.NodeOffline)
	place(t, in.Id, n.Id, 0)

	locs := locationsFor(in.Id)
	if len(locs) != 1 {
		t.Fatalf("an offline node's configs were withheld; got %d locations", len(locs))
	}
	if locs[0].Address != "203.0.113.10" {
		t.Errorf("address = %q; want the node's", locs[0].Address)
	}
}
