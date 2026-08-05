package sub

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/op/go-logging"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
)

func gateDB(t *testing.T) {
	t.Helper()
	// The gate logs a refusal, and logger.Infof panics on the package's nil
	// default. Every suite that exercises logging code initialises it the same
	// way (see web/controller/idor_test.go); CRITICAL keeps the output quiet.
	logger.InitLogger(logging.CRITICAL)
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
}

// seedSub stores one inbound holding one client, which is what accountForSub
// reads back out of the settings JSON.
func seedSub(t *testing.T, tag, email, subId string, limitDevices int) {
	t.Helper()
	settings := `{"clients":[{"email":"` + email + `","subId":"` + subId + `","limitDevices":` +
		itoa(limitDevices) + `,"id":"00000000-0000-0000-0000-000000000000"}]}`
	in := model.Inbound{Tag: tag, Protocol: model.VLESS, Port: 443, Enable: true, Settings: settings}
	if err := database.GetDB().Create(&in).Error; err != nil {
		t.Fatal(err)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// gateReq runs admitDevice for one request.
func gateReq(subId, userAgent, deviceId string) bool {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Params = gin.Params{{Key: "subid", Value: subId}}
	c.Request = httptest.NewRequest(http.MethodGet, "/sub/"+subId, nil)
	if userAgent != "" {
		c.Request.Header.Set("User-Agent", userAgent)
	}
	if deviceId != "" {
		c.Request.Header.Set("X-Device-Id", deviceId)
	}
	return admitDevice(c)
}

// The gate admits up to the limit and refuses past it.
func TestGateEnforcesTheLimit(t *testing.T) {
	gateDB(t)
	seedSub(t, "in-1", "customer@x", "SUB1", 2)

	if !gateReq("SUB1", "v2rayNG/1.8", "phone") {
		t.Fatal("first device refused")
	}
	if !gateReq("SUB1", "v2rayNG/1.8", "laptop") {
		t.Fatal("second device refused")
	}
	if gateReq("SUB1", "v2rayNG/1.8", "tablet") {
		t.Fatal("a third device was admitted against a limit of two")
	}
	// The devices already admitted keep working.
	if !gateReq("SUB1", "v2rayNG/1.8", "phone") {
		t.Fatal("an already-admitted device was refused on its next fetch")
	}
}

// No limit set means no gate. Most accounts will never have one, so this is the
// path that must stay cheap and must never refuse.
func TestGateIgnoresAccountsWithNoLimit(t *testing.T) {
	gateDB(t)
	seedSub(t, "in-1", "customer@x", "SUB1", 0)
	for i := 0; i < 10; i++ {
		if !gateReq("SUB1", "v2rayNG/1.8", "device-"+itoa(i)) {
			t.Fatalf("device %d refused on an unlimited account", i)
		}
	}
}

// A subId that matches nothing must be admitted, not refused.
//
// The gate is not an authorisation check -- GetSubs already errors on an unknown
// subId and returns nothing. Refusing here would only change the error a
// stranger sees, while a resolution failure on a REAL account would take a
// paying customer's configs away.
func TestGateFailsOpenOnUnknownSub(t *testing.T) {
	gateDB(t)
	if !gateReq("NO-SUCH-SUB", "v2rayNG/1.8", "phone") {
		t.Fatal("an unknown subId was refused; the gate must fail open")
	}
}

// One customer holding several protocols shares one device budget, because with
// a shared quota they share one email. Two configs on two inbounds, one limit.
func TestGateSharesOneBudgetAcrossProtocols(t *testing.T) {
	gateDB(t)
	seedSub(t, "wg-inbound", "customer@x", "SUB1", 1)
	seedSub(t, "ovpn-inbound", "customer@x", "SUB1", 1)

	if !gateReq("SUB1", "v2rayNG/1.8", "phone") {
		t.Fatal("first device refused")
	}
	// A second device is refused even though a second inbound exists: the budget
	// belongs to the account, not to each config.
	if gateReq("SUB1", "v2rayNG/1.8", "laptop") {
		t.Fatal("a second device was admitted because the customer had a second protocol")
	}
}

// The limit is read from whichever config carries a non-zero one, so setting it
// on any of a customer's protocols applies to all of them rather than depending
// on which inbound the query happened to return first.
func TestGateTakesTheLimitFromAnyConfig(t *testing.T) {
	gateDB(t)
	seedSub(t, "in-a", "customer@x", "SUB1", 0) // no limit here
	seedSub(t, "in-b", "customer@x", "SUB1", 1) // set here

	if !gateReq("SUB1", "v2rayNG/1.8", "phone") {
		t.Fatal("first device refused")
	}
	if gateReq("SUB1", "v2rayNG/1.8", "laptop") {
		t.Fatal("the limit set on one config was not applied to the account")
	}
}

// An app that sends no device header at all still gets counted, by User-Agent.
// Otherwise the limit would only work for the subset of clients that send one.
func TestGateFallsBackToUserAgent(t *testing.T) {
	gateDB(t)
	seedSub(t, "in-1", "customer@x", "SUB1", 1)

	if !gateReq("SUB1", "v2rayNG/1.8", "") {
		t.Fatal("first device refused")
	}
	if gateReq("SUB1", "Streisand/2.0", "") {
		t.Fatal("a different client app was admitted past a limit of one")
	}
	// The same app again is the same device.
	if !gateReq("SUB1", "v2rayNG/1.8", "") {
		t.Fatal("the same app was treated as a new device")
	}
}
