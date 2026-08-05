package sub

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-json"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/web/service"
)

// The device gate: the one point where a subscription request can be refused
// because the account already has as many devices as it is allowed.
//
// This is where it HAS to live. A device is only distinguishable at the moment a
// client app fetches its subscription and identifies itself in the request --
// nothing downstream in any of the protocols carries a device identity. See
// model.ClientDevice for what that means the limit actually enforces.

// deviceLimitMessage is what a refused device sees.
//
// It says what happened and what to do, because the person reading it is a
// paying customer looking at an app that suddenly stopped updating, not an
// operator reading a log. A bare 403 would send them to support with "it's
// broken", which costs the operator more than the extra device would have.
const deviceLimitMessage = "This subscription has reached its device limit. " +
	"Remove it from a device you no longer use, or ask your provider to raise the limit."

// deviceHeaders are the request headers a client app may use to identify itself
// beyond the User-Agent. Checked in order; the first non-empty one wins.
//
// None of these are standard. They are what real client apps send, and reading
// them costs nothing when absent -- whereas insisting on one would mean the
// limit only worked for a subset of apps.
var deviceHeaders = []string{
	"X-Device-Id",
	"X-Hwid",
	"X-Client-Id",
}

// admitDevice reports whether this request may be served, recording the device
// against the account.
//
// It FAILS OPEN on every internal problem: a subId that resolves to nothing, a
// database error, an account with no limit set. Refusing on an internal fault
// would turn a panel hiccup into every customer losing their configs at once,
// which is far worse than one extra device slipping through.
func admitDevice(c *gin.Context) bool {
	subId := c.Param("subid")
	if subId == "" {
		return true
	}

	email, limit, ok := accountForSub(subId)
	if !ok || limit <= 0 {
		return true
	}

	fingerprint := service.DeviceFingerprint(firstHeader(c, deviceHeaders), c.GetHeader("User-Agent"))
	var devices service.DeviceService
	admitted, err := devices.Admit(email, fingerprint, c.GetHeader("User-Agent"), c.ClientIP(), limit)
	if err != nil {
		logger.Warning("device gate: admitting after an error for", email, ":", err)
		return true
	}
	if !admitted {
		logger.Infof("device gate: refused a new device for %s (limit %d)", email, limit)
	}
	return admitted
}

func firstHeader(c *gin.Context, names []string) string {
	for _, n := range names {
		if v := strings.TrimSpace(c.GetHeader(n)); v != "" {
			return v
		}
	}
	return ""
}

// accountForSub resolves a subscription to the account that owns it and the
// device limit set on it.
//
// One subId can span several inbounds -- that is how one customer holds several
// protocols -- and with a shared quota they all carry the same email, so the
// first match is the account. The limit is read from whichever client carries a
// non-zero one, so setting it on any of the customer's configs applies to all of
// them rather than depending on which inbound happened to be found first.
func accountForSub(subId string) (email string, limit int, ok bool) {
	var inbounds []*model.Inbound
	err := database.GetDB().Model(model.Inbound{}).Where(`id in (
		SELECT DISTINCT inbounds.id
		FROM inbounds,
			JSON_EACH(JSON_EXTRACT(inbounds.settings, '$.clients')) AS client
		WHERE JSON_EXTRACT(client.value, '$.subId') = ? AND enable = ?
	)`, subId, true).Find(&inbounds).Error
	if err != nil {
		return "", 0, false
	}

	for _, in := range inbounds {
		var settings struct {
			Clients []model.Client `json:"clients"`
		}
		if json.Unmarshal([]byte(in.Settings), &settings) != nil {
			continue
		}
		for _, cl := range settings.Clients {
			if cl.SubID != subId {
				continue
			}
			if email == "" {
				email = cl.Email
			}
			if cl.LimitDevices > limit {
				limit = cl.LimitDevices
			}
			ok = true
		}
	}
	return email, limit, ok
}
