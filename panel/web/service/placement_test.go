package service

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
)

func placementFixture(t *testing.T) (svc *PlacementService, inboundId, localId int) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	in := model.Inbound{Tag: "wg", Protocol: model.WireGuard, Port: 51820, Enable: true}
	if err := database.GetDB().Create(&in).Error; err != nil {
		t.Fatal(err)
	}
	var local model.Node
	if err := database.GetDB().Where("is_local = ?", true).First(&local).Error; err != nil {
		t.Fatal(err)
	}
	return &PlacementService{}, in.Id, local.Id
}

func addNode(t *testing.T, name string) int {
	t.Helper()
	n := model.Node{Name: name, Address: "203.0.113.1", APIPort: 62050, Enable: true}
	if err := database.GetDB().Create(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n.Id
}

// The feature: choosing three nodes places the inbound on three nodes.
func TestSetPlacesOnEveryChosenNode(t *testing.T) {
	s, inboundId, localId := placementFixture(t)
	a, b := addNode(t, "frankfurt"), addNode(t, "helsinki")

	if err := s.SetForInbound(inboundId, []int{localId, a, b}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListForInbound(inboundId)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d placements; want 3", len(got))
	}
}

// Reconciling must not disturb rows that stay. A per-placement port override is
// operator configuration, and losing it on an unrelated edit to the list would
// silently move a node's daemon to a different port.
func TestSetPreservesUntouchedPlacements(t *testing.T) {
	s, inboundId, localId := placementFixture(t)
	a, b := addNode(t, "frankfurt"), addNode(t, "helsinki")

	if err := s.SetForInbound(inboundId, []int{localId, a}); err != nil {
		t.Fatal(err)
	}
	// Give Frankfurt its own port.
	placements, _ := s.ListForInbound(inboundId)
	var fraId int
	for _, p := range placements {
		if p.NodeId == a {
			fraId = p.Id
		}
	}
	if err := s.UpdatePlacement(fraId, 51999, "", true); err != nil {
		t.Fatal(err)
	}

	// Add Helsinki. Frankfurt's override must survive.
	if err := s.SetForInbound(inboundId, []int{localId, a, b}); err != nil {
		t.Fatal(err)
	}
	placements, _ = s.ListForInbound(inboundId)
	for _, p := range placements {
		if p.NodeId == a && p.Port != 51999 {
			t.Fatalf("Frankfurt's port override was lost on an unrelated edit: got %d", p.Port)
		}
	}
}

// Removing a node from the set removes its placement.
func TestSetRemovesUnchosenNodes(t *testing.T) {
	s, inboundId, localId := placementFixture(t)
	a := addNode(t, "frankfurt")

	if err := s.SetForInbound(inboundId, []int{localId, a}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetForInbound(inboundId, []int{localId}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListForInbound(inboundId)
	if len(got) != 1 || got[0].NodeId != localId {
		t.Fatalf("got %+v; want only the local placement", got)
	}
}

// An empty set is refused. It would leave the inbound served by nothing while
// the panel still lists it as enabled -- an outage that looks like a working
// configuration, which is the worst kind.
func TestSetRefusesAnEmptySet(t *testing.T) {
	s, inboundId, _ := placementFixture(t)
	err := s.SetForInbound(inboundId, nil)
	if err == nil {
		t.Fatal("removing every location was allowed")
	}
	if !strings.Contains(err.Error(), "disable") {
		t.Errorf("the error should point at the right control; got %v", err)
	}
}

// A node that does not exist is refused before anything is written, or a typo
// would leave the inbound half-placed.
func TestSetRefusesUnknownNodes(t *testing.T) {
	s, inboundId, localId := placementFixture(t)
	// Establish a known good state first. The fixture creates its inbound after
	// InitDB has already run the seeder, so it starts with no placements of its
	// own -- asserting against "unchanged" needs something to be unchanged FROM.
	if err := s.SetForInbound(inboundId, []int{localId}); err != nil {
		t.Fatal(err)
	}

	if err := s.SetForInbound(inboundId, []int{localId, 4242}); err == nil {
		t.Fatal("an unknown node id was accepted")
	}

	// The rejection must be total: validating every id BEFORE writing any is
	// what stops a typo leaving the inbound half-placed.
	got, _ := s.ListForInbound(inboundId)
	if len(got) != 1 || got[0].NodeId != localId {
		t.Fatalf("a rejected set altered the placements: %+v", got)
	}
}

// Disabling a placement and clearing a port override must both stick.
//
// This is the trap the models already hit once: GORM omits a STRUCT's zero
// values from an update, so Enable=false and Port=0 would both be silently
// dropped. UpdatePlacement uses a map for exactly that reason, and this is what
// would catch a future change back to a struct.
func TestUpdatePlacementPersistsZeroValues(t *testing.T) {
	s, inboundId, localId := placementFixture(t)
	if err := s.SetForInbound(inboundId, []int{localId}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListForInbound(inboundId)
	id := got[0].Id

	if err := s.UpdatePlacement(id, 51999, "10.0.0.1", true); err != nil {
		t.Fatal(err)
	}
	// Now back to "inherit the inbound's port" and disabled.
	if err := s.UpdatePlacement(id, 0, "", false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListForInbound(inboundId)
	if got[0].Port != 0 {
		t.Errorf("port override was not cleared; got %d", got[0].Port)
	}
	if got[0].Enable {
		t.Error("the placement was not disabled; GORM dropped the false")
	}
	if got[0].Listen != "" {
		t.Errorf("listen was not cleared; got %q", got[0].Listen)
	}
}

// A placement pointing at a deleted node is still listed, labelled, rather than
// hidden -- otherwise an operator sees an inbound served nowhere they can see
// and has nothing to click.
func TestListShowsPlacementsWithMissingNodes(t *testing.T) {
	s, inboundId, _ := placementFixture(t)
	if err := database.GetDB().Create(&model.InboundNode{
		InboundId: inboundId, NodeId: 4242, Enable: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	got, err := s.ListForInbound(inboundId)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, p := range got {
		if p.NodeId == 4242 && strings.Contains(p.NodeName, "missing") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a placement on a deleted node was hidden: %+v", got)
	}
}

// An out-of-range port is refused rather than written.
func TestUpdatePlacementRangeChecksPort(t *testing.T) {
	s, inboundId, localId := placementFixture(t)
	_ = s.SetForInbound(inboundId, []int{localId})
	got, _ := s.ListForInbound(inboundId)
	if err := s.UpdatePlacement(got[0].Id, 99999, "", true); err == nil {
		t.Fatal("an out-of-range port was accepted")
	}
}
