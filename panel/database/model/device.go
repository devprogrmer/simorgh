package model

// ClientDevice is one device that has fetched a subscription.
//
// What this can and cannot do is worth stating precisely, because "HWID limit"
// promises more than any VPN protocol can deliver. None of them carry a hardware
// identity: WireGuard has a public key, OpenVPN a certificate, L2TP a username.
// The only moment a DEVICE is distinguishable is when a client app fetches the
// subscription, where it identifies itself in the request.
//
// So this limits how many devices may RETRIEVE the config, not how many may use
// one once retrieved -- someone who copies a config by hand is not counted. That
// is the same thing panels advertising an HWID limit actually enforce, and it is
// genuinely useful: it stops an account being pasted into a group chat, which is
// the sharing that actually happens.
type ClientDevice struct {
	Id int `json:"id" gorm:"primaryKey;autoIncrement"`

	// Email is the account identity, matching client_traffics. Not the subId:
	// quota, expiry and the speed limit are all keyed on email, and a device
	// limit that used a different key would drift out of step with them.
	Email string `json:"email" gorm:"index:idx_device_email_fp,priority:1;index"`

	// Fingerprint identifies the device as well as an HTTP request allows. See
	// DeviceFingerprint for what goes into it and why.
	Fingerprint string `json:"fingerprint" gorm:"index:idx_device_email_fp,priority:2"`

	// Name is what to show an operator: the client app as it announced itself.
	// Raw User-Agent rather than a parsed guess, because a wrong guess is worse
	// than a long string when someone is deciding which device to revoke.
	Name string `json:"name"`

	FirstSeen int64  `json:"firstSeen"`
	LastSeen  int64  `json:"lastSeen"`
	LastIP    string `json:"lastIp"`
}

// The unique index on (email, fingerprint) is what makes the count meaningful:
// without it a device that fetches its subscription every hour would register as
// a new one each time and exhaust the limit by itself.
func (ClientDevice) TableName() string { return "client_devices" }
