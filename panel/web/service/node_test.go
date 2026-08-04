package service

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
)

func newNodeService(t *testing.T) *NodeService {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	return &NodeService{}
}

func localNodeId(t *testing.T) int {
	t.Helper()
	var n model.Node
	if err := database.GetDB().Where("is_local = ?", true).First(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n.Id
}

// The local node is driven in-process; anything else goes over the wire. Getting
// this backwards would either send the master's own host through a network round
// trip to itself, or try to drive a remote machine by editing local files.
func TestNodeServiceRunnerForPicksImplementation(t *testing.T) {
	s := newNodeService(t)

	r, err := s.RunnerFor(localNodeId(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.(*LocalRunner); !ok {
		t.Errorf("local node got %T; want *LocalRunner", r)
	}

	remote := model.Node{Name: "fra", Address: "203.0.113.10", APIPort: 62050, Enable: true}
	if err := database.GetDB().Create(&remote).Error; err != nil {
		t.Fatal(err)
	}
	// A remote node with no certificate cannot be dialled, and that must be an
	// error rather than a silent fallback to the local runner -- which would
	// apply another machine's configuration to this one.
	if _, err := s.RunnerFor(remote.Id); err == nil {
		t.Error("a remote node without certificate material must not produce a runner")
	}
}

// Three consecutive failures mark a node offline, not one. A single slow tick on
// a link to another country is not an outage, and flapping a node's status
// flaps every inbound on it in the UI.
func TestNodeServiceOfflineAfterThreeFailures(t *testing.T) {
	s := newNodeService(t)
	remote := model.Node{Name: "fra", Address: "203.0.113.10", APIPort: 62050, Enable: true, Status: model.NodeOnline}
	if err := database.GetDB().Create(&remote).Error; err != nil {
		t.Fatal(err)
	}

	boom := errTestDial
	for i := 1; i <= 2; i++ {
		s.MarkResult(remote.Id, boom)
		var got model.Node
		database.GetDB().First(&got, remote.Id)
		if got.Status == model.NodeOffline {
			t.Fatalf("node went offline after %d failure(s); want it to tolerate %d",
				i, model.NodeOfflineAfterFailures-1)
		}
	}
	s.MarkResult(remote.Id, boom)
	var got model.Node
	database.GetDB().First(&got, remote.Id)
	if got.Status != model.NodeOffline {
		t.Fatalf("status after %d failures = %q; want offline", model.NodeOfflineAfterFailures, got.Status)
	}
	if got.LastError == "" {
		t.Error("an offline node must carry why, or the operator has nothing to act on")
	}
}

// One success clears the streak. Without this a node that failed twice hours ago
// would be one blip away from being declared down.
func TestNodeServiceSuccessResetsFailureStreak(t *testing.T) {
	s := newNodeService(t)
	remote := model.Node{Name: "fra", Address: "203.0.113.10", APIPort: 62050, Enable: true}
	if err := database.GetDB().Create(&remote).Error; err != nil {
		t.Fatal(err)
	}

	s.MarkResult(remote.Id, errTestDial)
	s.MarkResult(remote.Id, errTestDial)
	s.MarkResult(remote.Id, nil)

	var got model.Node
	database.GetDB().First(&got, remote.Id)
	if got.Status != model.NodeOnline {
		t.Errorf("status after a success = %q; want online", got.Status)
	}
	if got.LastError != "" {
		t.Errorf("LastError should clear on success, got %q", got.LastError)
	}
	if got.LastSeen == 0 {
		t.Error("a successful contact must record when")
	}

	// Two more failures must still not be enough, proving the counter reset.
	s.MarkResult(remote.Id, errTestDial)
	s.MarkResult(remote.Id, errTestDial)
	database.GetDB().First(&got, remote.Id)
	if got.Status == model.NodeOffline {
		t.Error("the failure counter did not reset on success")
	}
}

// Key material must never leave the process. List is what the API returns, and a
// node's private key in an admin's browser is a node an attacker can impersonate.
func TestNodeServiceListNeverLeaksKeys(t *testing.T) {
	s := newNodeService(t)
	remote := model.Node{
		Name: "fra", Address: "203.0.113.10", APIPort: 62050, Enable: true,
		ServerCert: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
		ServerKey:  "-----BEGIN EC PRIVATE KEY-----\nSECRETMATERIAL\n-----END EC PRIVATE KEY-----",
	}
	if err := database.GetDB().Create(&remote).Error; err != nil {
		t.Fatal(err)
	}

	nodes, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(nodes)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, forbidden := range []string{"SECRETMATERIAL", "PRIVATE KEY", "BEGIN CERTIFICATE"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("List output contains %q:\n%s", forbidden, body)
		}
	}
}

// The local node cannot be deleted. It represents the panel's own host, so
// removing it would leave every inbound placed there served by nothing, with no
// way to put them back from the UI.
func TestNodeServiceRefusesToDeleteLocalNode(t *testing.T) {
	s := newNodeService(t)
	if err := s.Delete(localNodeId(t)); err == nil {
		t.Fatal("deleting the local node was allowed")
	}
}

// Deleting a node that still carries inbounds must say which, rather than
// silently orphaning them.
func TestNodeServiceDeleteReportsPlacements(t *testing.T) {
	s := newNodeService(t)
	remote := model.Node{Name: "fra", Address: "203.0.113.10", APIPort: 62050, Enable: true}
	if err := database.GetDB().Create(&remote).Error; err != nil {
		t.Fatal(err)
	}
	in := model.Inbound{Tag: "on-fra", Enable: true, Port: 443}
	if err := database.GetDB().Create(&in).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Create(&model.InboundNode{InboundId: in.Id, NodeId: remote.Id, Enable: true}).Error; err != nil {
		t.Fatal(err)
	}

	err := s.Delete(remote.Id)
	if err == nil {
		t.Fatal("deleting a node with placements was allowed silently")
	}
	if !strings.Contains(err.Error(), "on-fra") {
		t.Errorf("the error must name the affected inbound; got %v", err)
	}
}

// The CA is generated once and reused. A second CA would invalidate every node
// certificate already issued, taking the whole fleet offline at once.
func TestNodeServiceCAIsStable(t *testing.T) {
	s := newNodeService(t)
	cert1, key1, err := s.ensureCA()
	if err != nil {
		t.Fatal(err)
	}
	cert2, key2, err := s.ensureCA()
	if err != nil {
		t.Fatal(err)
	}
	if string(cert1) != string(cert2) || string(key1) != string(key2) {
		t.Fatal("ensureCA minted a second authority; every issued node certificate would stop verifying")
	}
	if len(cert1) == 0 || len(key1) == 0 {
		t.Fatal("ensureCA returned empty material")
	}
}
