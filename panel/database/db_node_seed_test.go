package database

import (
	"path/filepath"
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database/model"
)

// initTestDB opens a throwaway panel database, which also runs seedLocalNode
// once as part of InitDB.
func initTestDB(t *testing.T) {
	t.Helper()
	if err := InitDB(filepath.Join(t.TempDir(), "vpn-ui.db")); err != nil {
		t.Fatal(err)
	}
}

func countNodes(t *testing.T) (local int64, placements int64) {
	t.Helper()
	if err := db.Model(&model.Node{}).Where("is_local = ?", true).Count(&local).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.InboundNode{}).Count(&placements).Error; err != nil {
		t.Fatal(err)
	}
	return local, placements
}

// A fresh panel gets exactly one local node, and running the seeder again does
// not add a second. seedLocalNode runs on EVERY start (it is not recorded in
// history_of_seeders), so non-idempotency here would mean a new duplicate row
// per boot.
func TestSeedLocalNodeIsIdempotent(t *testing.T) {
	initTestDB(t)

	for i := 0; i < 3; i++ {
		if err := seedLocalNode(); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	local, _ := countNodes(t)
	if local != 1 {
		t.Fatalf("local nodes = %d after repeated seeding; want exactly 1", local)
	}
}

// The upgrade path that must be invisible: a panel that already has inbounds
// gets each of them placed on the local node, exactly once, so they keep being
// served after the upgrade instead of belonging to no node at all.
func TestSeedLocalNodePlacesExistingInbounds(t *testing.T) {
	initTestDB(t)

	for _, tag := range []string{"in-1", "in-2"} {
		if err := db.Create(&model.Inbound{Tag: tag, Enable: true, Port: 443}).Error; err != nil {
			t.Fatal(err)
		}
	}

	if err := seedLocalNode(); err != nil {
		t.Fatal(err)
	}
	if err := seedLocalNode(); err != nil {
		t.Fatal(err)
	}

	local, placements := countNodes(t)
	if local != 1 {
		t.Fatalf("local nodes = %d; want 1", local)
	}
	if placements != 2 {
		t.Fatalf("placements = %d; want one per inbound (2), so seeding twice must not double them", placements)
	}
}

// An inbound already placed on a REMOTE node must not also be placed locally.
// Getting this wrong would silently start serving a foreign node's inbound on
// the panel host as well -- the same clients answered from two places, and
// double-billed.
func TestSeedLocalNodeLeavesRemotePlacementsAlone(t *testing.T) {
	initTestDB(t)

	remote := model.Node{Name: "frankfurt", Address: "203.0.113.10", APIPort: 62050, Enable: true}
	if err := db.Create(&remote).Error; err != nil {
		t.Fatal(err)
	}
	in := model.Inbound{Tag: "remote-only", Enable: true, Port: 443}
	if err := db.Create(&in).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.InboundNode{InboundId: in.Id, NodeId: remote.Id, Enable: true}).Error; err != nil {
		t.Fatal(err)
	}

	if err := seedLocalNode(); err != nil {
		t.Fatal(err)
	}

	var placements []model.InboundNode
	if err := db.Where("inbound_id = ?", in.Id).Find(&placements).Error; err != nil {
		t.Fatal(err)
	}
	if len(placements) != 1 {
		t.Fatalf("placements for a remote-only inbound = %d; want 1, the seeder must not add a local copy", len(placements))
	}
	if placements[0].NodeId != remote.Id {
		t.Fatalf("placement moved to node %d; want it left on the remote node %d", placements[0].NodeId, remote.Id)
	}
}

// The local row is recreated if it goes missing, which is why the seeder runs
// every start rather than once. Without this a hand-edited or partially
// restored database would leave the panel with no local node, and every inbound
// placed on it unserved.
func TestSeedLocalNodeRecreatesMissingLocalRow(t *testing.T) {
	initTestDB(t)

	if err := db.Where("is_local = ?", true).Delete(&model.Node{}).Error; err != nil {
		t.Fatal(err)
	}
	if local, _ := countNodes(t); local != 0 {
		t.Fatalf("setup: local nodes = %d, want 0", local)
	}

	if err := seedLocalNode(); err != nil {
		t.Fatal(err)
	}
	if local, _ := countNodes(t); local != 1 {
		t.Fatalf("local nodes = %d after reseeding; want the row restored", local)
	}
}

// The unique index on (inbound_id, node_id) is the last line of defence against
// a double placement, and it has to hold even when a caller bypasses the
// seeder's own guard -- ImportDB swaps the database file wholesale, so no
// service-level check sees that data at all.
func TestPlacementUniquePerInboundAndNode(t *testing.T) {
	initTestDB(t)

	in := model.Inbound{Tag: "dup", Enable: true, Port: 443}
	if err := db.Create(&in).Error; err != nil {
		t.Fatal(err)
	}
	var local model.Node
	if err := db.Where("is_local = ?", true).First(&local).Error; err != nil {
		t.Fatal(err)
	}

	// The inbound was created after InitDB, so nothing has placed it yet: this
	// first placement is legitimate and must succeed.
	if err := db.Create(&model.InboundNode{InboundId: in.Id, NodeId: local.Id, Enable: true}).Error; err != nil {
		t.Fatalf("the first placement must be accepted: %v", err)
	}
	// The same pair a second time must not be.
	if err := db.Create(&model.InboundNode{InboundId: in.Id, NodeId: local.Id, Enable: true}).Error; err == nil {
		t.Fatal("a duplicate (inbound, node) placement was accepted; the unique index is missing")
	}

	// Different node, same inbound, is the multi-location case and must still be
	// allowed -- the index constrains the PAIR, not either column.
	other := model.Node{Name: "helsinki", Address: "203.0.113.20", APIPort: 62050, Enable: true}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.InboundNode{InboundId: in.Id, NodeId: other.Id, Enable: true}).Error; err != nil {
		t.Fatalf("placing one inbound on a second node must be allowed, that is the point: %v", err)
	}
}
