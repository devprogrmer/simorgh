package service

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
)

// DeviceService counts and caps the devices allowed to fetch one account's
// subscription. See model.ClientDevice for what that does and does not enforce.
type DeviceService struct{}

// DeviceFingerprint derives a stable identifier for the requesting device.
//
// The inputs are chosen for STABILITY first. A fingerprint that changes between
// two fetches from the same phone would let one device exhaust the whole limit
// on its own, which is a worse failure than a loose match: the customer is
// locked out of an account they are using correctly, and nothing tells them why.
//
//   - An explicit device header, when the app sends one. Some clients do, and it
//     is the only genuinely per-device value available.
//   - Otherwise the User-Agent, which carries the app and its version.
//
// The IP is deliberately NOT included. Phones change address constantly -- cell
// to wifi, one cell tower to the next -- and every change would look like a new
// device. It is stored alongside for the operator to look at, not hashed in.
//
// Two identical phones running the same app version will collide and count as
// one device. That is the honest limit of what HTTP reveals, and it errs toward
// admitting a real customer rather than refusing one.
func DeviceFingerprint(deviceHeader, userAgent string) string {
	raw := strings.TrimSpace(deviceHeader)
	if raw == "" {
		raw = strings.TrimSpace(userAgent)
	}
	if raw == "" {
		// Nothing to go on. A shared constant is better than an empty string,
		// which would make every anonymous fetch a separate device.
		raw = "unidentified-client"
	}
	sum := sha256.Sum256([]byte(strings.ToLower(raw)))
	return hex.EncodeToString(sum[:16])
}

// Admit records a device against an account and reports whether it may proceed.
//
// A device already known is always admitted, whatever the limit is now: lowering
// a limit must not lock out devices already using the account, or an operator
// tightening a plan would cut off customers mid-month with no warning.
//
// limit <= 0 means no limit, matching how LimitIP and the other caps in this
// schema read zero.
func (s *DeviceService) Admit(email, fingerprint, name, ip string, limit int) (bool, error) {
	if email == "" || fingerprint == "" {
		return true, nil
	}
	db := database.GetDB()
	now := time.Now().Unix()

	var existing model.ClientDevice
	err := db.Where("email = ? AND fingerprint = ?", email, fingerprint).First(&existing).Error
	if err == nil {
		db.Model(&existing).Updates(map[string]any{
			"last_seen": now,
			"last_ip":   ip,
			"name":      name,
		})
		return true, nil
	}

	if limit > 0 {
		var count int64
		if err := db.Model(&model.ClientDevice{}).Where("email = ?", email).Count(&count).Error; err != nil {
			// A counting failure admits rather than refuses. Refusing on an
			// internal error would turn a database hiccup into every customer
			// being locked out at once, which is far worse than one extra device.
			return true, err
		}
		if count >= int64(limit) {
			return false, nil
		}
	}

	return true, db.Create(&model.ClientDevice{
		Email:       email,
		Fingerprint: fingerprint,
		Name:        name,
		FirstSeen:   now,
		LastSeen:    now,
		LastIP:      ip,
	}).Error
}

// List returns an account's known devices, most recently seen first, which is
// the order an operator revoking one wants.
func (s *DeviceService) List(email string) ([]model.ClientDevice, error) {
	var devices []model.ClientDevice
	err := database.GetDB().Where("email = ?", email).
		Order("last_seen DESC").Find(&devices).Error
	return devices, err
}

// Forget removes one device, so a customer who replaced a phone can use the slot
// again without their limit being raised.
func (s *DeviceService) Forget(id int) error {
	return database.GetDB().Delete(&model.ClientDevice{}, id).Error
}

// ForgetAll clears an account's devices. The reset an operator reaches for when
// a customer says they have changed everything.
func (s *DeviceService) ForgetAll(email string) error {
	return database.GetDB().Where("email = ?", email).Delete(&model.ClientDevice{}).Error
}
