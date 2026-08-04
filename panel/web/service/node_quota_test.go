package service

import (
	"path/filepath"
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

const oneGiB = int64(1024 * 1024 * 1024)

// The reason accounting stays central, asserted directly.
//
// One account placed on three nodes, each reporting 4 GB against a 10 GB quota,
// must end DISABLED at 12 GB total -- not sail past because each node only ever
// saw 4 of its own. Enforcing per node is the obvious-looking design and it
// gives a customer three times what they paid for.
func TestQuotaIsSharedAcrossNodes(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()

	in := model.Inbound{Tag: "shared", Protocol: model.WireGuard, Port: 51820, Enable: true, Total: 0}
	if err := db.Create(&in).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&xray.ClientTraffic{
		InboundId: in.Id, Email: "customer@example.com", Enable: true, Total: 10 * oneGiB,
	}).Error; err != nil {
		t.Fatal(err)
	}

	// Three nodes each report the same account moving 4 GB in one tick. This is
	// exactly the shape the master receives from three Collect calls.
	var reports []*xray.ClientTraffic
	for i := 0; i < 3; i++ {
		reports = append(reports, &xray.ClientTraffic{
			InboundId: in.Id, Email: "customer@example.com", Enable: true,
			Up: 2 * oneGiB, Down: 2 * oneGiB,
		})
	}

	var svc InboundService
	err, _, _, _, _ := svc.AddTraffic(nil, reports)
	if err != nil {
		t.Fatalf("AddTraffic: %v", err)
	}

	var got xray.ClientTraffic
	if err := db.Where("email = ?", "customer@example.com").First(&got).Error; err != nil {
		t.Fatal(err)
	}

	total := got.Up + got.Down
	if total != 12*oneGiB {
		t.Fatalf("billed %d bytes; want all three nodes' 12 GiB summed centrally", total)
	}
	if got.Enable {
		t.Fatalf("account still enabled at %d bytes against a %d byte quota; "+
			"per-node enforcement would have let it spend the quota three times", total, got.Total)
	}
}

// The de-duplication guard has to survive the move into LocalRunner.Collect.
//
// MTProto and SSH keep their own byte counters AND egress through a paired Xray
// socks inbound, so the same transfer arrives twice. The predicate is presence,
// not byte count: the two sources flush on different boundaries, so an account
// Xray reports as zero this tick may have its bytes reported a tick later, and
// gating on a positive count bills the transfer twice.
func TestRelayTrafficDeduplicationSurvivesTheMove(t *testing.T) {
	xrayReported := []*xray.ClientTraffic{
		{Email: "a@x", Up: 0, Down: 0},   // seen by Xray, zero bytes THIS tick
		{Email: "b@x", Up: 100, Down: 0}, // seen by Xray with bytes
	}
	relayReported := []*xray.ClientTraffic{
		{Email: "a@x", Up: 5000, Down: 0}, // the relay tallied what Xray will report later
		{Email: "b@x", Up: 7000, Down: 0}, // same transfer Xray already billed
		{Email: "c@x", Up: 900, Down: 0},  // relay-only account Xray never tracks
	}

	out := appendUnrecordedTraffic(xrayReported, relayReported)

	byEmail := map[string]int{}
	for _, tr := range out {
		byEmail[tr.Email]++
	}
	if byEmail["a@x"] != 1 {
		t.Errorf("a@x appears %d times; a zero-byte Xray record still counts as present, "+
			"or the transfer is billed twice", byEmail["a@x"])
	}
	if byEmail["b@x"] != 1 {
		t.Errorf("b@x appears %d times; want 1", byEmail["b@x"])
	}
	if byEmail["c@x"] != 1 {
		t.Errorf("c@x appears %d times; a relay-only account must still be billed", byEmail["c@x"])
	}
}

// Two relays reporting the same account in one tick contribute once.
func TestRelayDeduplicationWithinOneTick(t *testing.T) {
	out := appendUnrecordedTraffic(nil, []*xray.ClientTraffic{
		{Email: "dup@x", Up: 10},
		{Email: "dup@x", Up: 20},
	})
	if len(out) != 1 {
		t.Fatalf("got %d records for one account in one tick; want 1", len(out))
	}
}
