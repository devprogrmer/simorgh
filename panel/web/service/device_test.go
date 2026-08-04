package service

import (
	"path/filepath"
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database"
)

func deviceDB(t *testing.T) *DeviceService {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	return &DeviceService{}
}

// The core of the feature: a limit of two admits two devices and refuses a
// third.
func TestDeviceLimitRefusesBeyondTheCap(t *testing.T) {
	s := deviceDB(t)
	for i, fp := range []string{"phone", "laptop"} {
		ok, err := s.Admit("customer@x", fp, "app/1.0", "203.0.113.1", 2)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("device %d was refused inside the limit", i+1)
		}
	}
	ok, err := s.Admit("customer@x", "tablet", "app/1.0", "203.0.113.1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a third device was admitted against a limit of two")
	}
}

// A device that fetches its subscription repeatedly must not consume a new slot
// each time. Client apps refresh on a timer, so without this a single phone
// exhausts any limit within hours.
func TestRepeatedFetchesAreOneDevice(t *testing.T) {
	s := deviceDB(t)
	for i := 0; i < 20; i++ {
		ok, err := s.Admit("customer@x", "phone", "app/1.0", "203.0.113.1", 1)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("the same device was refused on fetch %d", i+1)
		}
	}
	devices, err := s.List("customer@x")
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("20 fetches from one device produced %d rows", len(devices))
	}
}

// Lowering a limit must not lock out devices already using the account. An
// operator tightening a plan would otherwise cut customers off mid-month with
// nothing telling them why.
func TestKnownDeviceIsAdmittedAfterTheLimitDrops(t *testing.T) {
	s := deviceDB(t)
	for _, fp := range []string{"phone", "laptop", "tablet"} {
		if ok, err := s.Admit("customer@x", fp, "app/1.0", "203.0.113.1", 3); err != nil || !ok {
			t.Fatalf("setup: %v %v", ok, err)
		}
	}
	// The limit is now 1, but all three are already known.
	for _, fp := range []string{"phone", "laptop", "tablet"} {
		ok, err := s.Admit("customer@x", fp, "app/1.0", "203.0.113.1", 1)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("known device %q was locked out when the limit dropped", fp)
		}
	}
	// A genuinely new one is still refused.
	if ok, _ := s.Admit("customer@x", "newphone", "app/1.0", "203.0.113.1", 1); ok {
		t.Error("a new device was admitted past the lowered limit")
	}
}

// Zero means no limit, matching how LimitIP and the other caps in this schema
// read zero.
func TestZeroLimitMeansUnlimited(t *testing.T) {
	s := deviceDB(t)
	for i := 0; i < 25; i++ {
		ok, err := s.Admit("customer@x", string(rune('a'+i))+"-device", "app/1.0", "203.0.113.1", 0)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("device %d refused under a zero (unlimited) limit", i)
		}
	}
}

// Two accounts do not share a device budget.
func TestDeviceCountsArePerAccount(t *testing.T) {
	s := deviceDB(t)
	if ok, _ := s.Admit("a@x", "phone", "app/1.0", "203.0.113.1", 1); !ok {
		t.Fatal("first account refused")
	}
	if ok, _ := s.Admit("b@x", "phone", "app/1.0", "203.0.113.1", 1); !ok {
		t.Fatal("a second account was refused because another account had a device")
	}
}

// Forgetting a device frees its slot, which is what an operator does when a
// customer replaces a phone.
func TestForgetFreesASlot(t *testing.T) {
	s := deviceDB(t)
	if ok, _ := s.Admit("customer@x", "oldphone", "app/1.0", "203.0.113.1", 1); !ok {
		t.Fatal("setup")
	}
	if ok, _ := s.Admit("customer@x", "newphone", "app/1.0", "203.0.113.1", 1); ok {
		t.Fatal("setup: the second device should have been refused")
	}
	devices, _ := s.List("customer@x")
	if err := s.Forget(devices[0].Id); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Admit("customer@x", "newphone", "app/1.0", "203.0.113.1", 1); !ok {
		t.Fatal("the freed slot was not usable")
	}
}

// The fingerprint must be STABLE across the things that change between two
// fetches from one device, and must distinguish devices that really differ.
func TestFingerprintStability(t *testing.T) {
	// The same app is the same device regardless of case.
	if DeviceFingerprint("", "v2rayNG/1.8.1") != DeviceFingerprint("", "v2rayng/1.8.1") {
		t.Error("case changed the fingerprint")
	}
	// A different app is a different device.
	if DeviceFingerprint("", "v2rayNG/1.8.1") == DeviceFingerprint("", "Streisand/2.0") {
		t.Error("two different clients share a fingerprint")
	}
	// An explicit device header wins over the User-Agent, since it is the only
	// genuinely per-device value available.
	a := DeviceFingerprint("device-abc", "v2rayNG/1.8.1")
	b := DeviceFingerprint("device-xyz", "v2rayNG/1.8.1")
	if a == b {
		t.Error("the device header was ignored; two devices on one app version collide")
	}
	// Nothing to go on still yields a value, or every anonymous fetch would look
	// like a separate device.
	if DeviceFingerprint("", "") == "" {
		t.Error("an unidentified client produced an empty fingerprint")
	}
}

// The IP is deliberately not part of the fingerprint: phones change address
// constantly, and every change would register a new device and exhaust the
// limit within a day of normal use.
func TestFingerprintIgnoresAddress(t *testing.T) {
	s := deviceDB(t)
	for _, ip := range []string{"203.0.113.1", "198.51.100.7", "192.0.2.44"} {
		ok, err := s.Admit("customer@x", DeviceFingerprint("", "v2rayNG/1.8.1"), "v2rayNG/1.8.1", ip, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("the same device was refused after moving to %s", ip)
		}
	}
	devices, _ := s.List("customer@x")
	if len(devices) != 1 {
		t.Fatalf("one device across three addresses produced %d rows", len(devices))
	}
	if devices[0].LastIP != "192.0.2.44" {
		t.Errorf("the latest address should still be recorded for the operator, got %q", devices[0].LastIP)
	}
}
