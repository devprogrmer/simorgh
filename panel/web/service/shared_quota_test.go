package service

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

func quotaDB(t *testing.T) {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
}

// makeInbound stores an inbound carrying one client, the way the panel does.
func makeInbound(t *testing.T, tag string, proto model.Protocol, email, subId string) {
	t.Helper()
	settings, err := json.Marshal(map[string]any{
		"clients": []map[string]any{{"email": email, "subId": subId, "id": email}},
	})
	if err != nil {
		t.Fatal(err)
	}
	in := model.Inbound{Tag: tag, Protocol: proto, Port: 0, Enable: true, Settings: string(settings)}
	if err := database.GetDB().Create(&in).Error; err != nil {
		t.Fatal(err)
	}
}

// One customer, many protocols, ONE quota.
//
// This is the shape an operator actually sells: give someone 20 GB and let them
// spend it across whichever of the protocols they were given. Before, the panel
// refused the second protocol outright ("emails must be unique across all
// inbounds"), so the same customer needed a separate account per protocol and
// therefore a separate quota -- a 20 GB customer on three protocols could spend
// 60.
func TestSameEmailAcrossProtocolsWhenSubIdMatches(t *testing.T) {
	quotaDB(t)
	var svc InboundService

	makeInbound(t, "wg", model.WireGuard, "customer@x", "SUB-1")
	makeInbound(t, "ovpn", model.OPENVPN, "customer@x", "SUB-1")
	makeInbound(t, "l2tp", model.L2TP, "customer@x", "SUB-1")

	// A fourth protocol for the same subscription must be accepted.
	dup, err := svc.checkEmailsExistForClients([]model.Client{
		{Email: "customer@x", SubID: "SUB-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dup != "" {
		t.Fatalf("the same customer on another protocol was refused as a duplicate: %q", dup)
	}
}

// The protection that must survive: two different customers cannot share an
// identity. Quota, expiry and the speed limit are all keyed on email, so
// allowing it would pool two people's traffic into one account.
func TestSameEmailUnderDifferentSubIdIsStillRefused(t *testing.T) {
	quotaDB(t)
	var svc InboundService

	makeInbound(t, "wg", model.WireGuard, "customer@x", "SUB-1")

	dup, err := svc.checkEmailsExistForClients([]model.Client{
		{Email: "customer@x", SubID: "SUB-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dup != "customer@x" {
		t.Fatal("two different subscriptions were allowed to share an email; their quotas would pool")
	}
}

// An empty subId must not act as a wildcard. Clients created without one are the
// historic default, so treating "" as a match would let any two of them share an
// email -- reintroducing the exact failure for the most common case.
func TestEmptySubIdIsNotAWildcard(t *testing.T) {
	quotaDB(t)
	var svc InboundService

	makeInbound(t, "wg", model.WireGuard, "customer@x", "")

	dup, err := svc.checkEmailsExistForClients([]model.Client{
		{Email: "customer@x", SubID: ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dup != "customer@x" {
		t.Fatal("two subId-less clients were allowed to share an email")
	}
}

// The rule applies within a single batch too, so one edit can put the same
// account on several protocols at once.
func TestBatchAllowsSameSubscriptionAndRefusesOthers(t *testing.T) {
	quotaDB(t)
	var svc InboundService

	dup, err := svc.checkEmailsExistForClients([]model.Client{
		{Email: "customer@x", SubID: "SUB-1"},
		{Email: "customer@x", SubID: "SUB-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dup != "" {
		t.Fatalf("one batch could not place the same subscription on two protocols: %q", dup)
	}

	dup, err = svc.checkEmailsExistForClients([]model.Client{
		{Email: "customer@x", SubID: "SUB-1"},
		{Email: "customer@x", SubID: "SUB-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dup != "customer@x" {
		t.Fatal("a batch mixing two subscriptions on one email was accepted")
	}
}

// The consequence, end to end: traffic reported against that email from SEVERAL
// protocols lands in one row and one quota.
//
// 8 GB over WireGuard, 7 over OpenVPN and 6 over L2TP is 21 against a 20 GB
// account, so it ends disabled -- not three protocols each with 20 GB of room.
func TestOneQuotaAcrossEveryProtocol(t *testing.T) {
	quotaDB(t)
	db := database.GetDB()

	in := model.Inbound{Tag: "wg", Protocol: model.WireGuard, Enable: true, Port: 51820}
	if err := db.Create(&in).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&xray.ClientTraffic{
		InboundId: in.Id, Email: "customer@x", Enable: true, Total: 20 * oneGiB,
	}).Error; err != nil {
		t.Fatal(err)
	}

	// Three protocols reporting the same account in one tick.
	reports := []*xray.ClientTraffic{
		{InboundId: in.Id, Email: "customer@x", Enable: true, Up: 4 * oneGiB, Down: 4 * oneGiB},
		{InboundId: in.Id, Email: "customer@x", Enable: true, Up: 3 * oneGiB, Down: 4 * oneGiB},
		{InboundId: in.Id, Email: "customer@x", Enable: true, Up: 3 * oneGiB, Down: 3 * oneGiB},
	}

	var svc InboundService
	if err, _, _, _, _ := svc.AddTraffic(nil, reports); err != nil {
		t.Fatalf("AddTraffic: %v", err)
	}

	var got xray.ClientTraffic
	if err := db.Where("email = ?", "customer@x").First(&got).Error; err != nil {
		t.Fatal(err)
	}
	if total := got.Up + got.Down; total != 21*oneGiB {
		t.Fatalf("billed %d bytes; want every protocol summed into one account (21 GiB)", total)
	}
	if got.Enable {
		t.Fatal("the account is still enabled past its shared 20 GB quota")
	}
}
