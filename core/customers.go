package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// customer is one entry from CUSTOMERS_FILE. Each customer's password
// deterministically derives a distinct sessID (see sessionIDFromPassword),
// which is what the server uses to tell customers' traffic apart on the
// wire - no protocol change was needed for this, only server-side
// bookkeeping.
type customer struct {
	Name          string  `json:"name"`
	Password      string  `json:"password"`
	BandwidthMbps float64 `json:"bandwidth_mbps,omitempty"` // 0 = unlimited
	sessID        uint16
}

// loadCustomers reads and validates the JSON customer list. File format:
//
//	[
//	  {"name": "alice", "password": "...", "bandwidth_mbps": 10},
//	  {"name": "bob",   "password": "..."}
//	]
//
// bandwidth_mbps is optional; omit it (or set 0) for no per-customer cap.
// This is a real, working rate limit enforced by Simorgh's own token
// bucket on the carrier hop - it exists specifically because the
// underlying VPN protocols/panels (WireGuard, Xray-based panels like
// 3X-UI/vpn-ui) do not have a built-in equivalent as of this writing; see
// docs/DEPLOYMENT.md for the sourcing on that gap.
func loadCustomers(path string) ([]*customer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var list []*customer
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("%s contains no customers", path)
	}

	seenNames := make(map[string]bool, len(list))
	seenSessIDs := make(map[uint16]string, len(list))
	for _, c := range list {
		if c.Name == "" {
			return nil, fmt.Errorf("a customer entry is missing \"name\"")
		}
		if c.Password == "" {
			return nil, fmt.Errorf("customer %q is missing \"password\"", c.Name)
		}
		if seenNames[c.Name] {
			return nil, fmt.Errorf("duplicate customer name %q", c.Name)
		}
		seenNames[c.Name] = true

		c.sessID = sessionIDFromPassword(c.Password)
		if other, collide := seenSessIDs[c.sessID]; collide {
			// Astronomically unlikely with real random passwords (1-in-65536
			// per pair), but if it ever happens two customers would be
			// indistinguishable on the wire - refuse to start rather than
			// silently mixing their traffic.
			return nil, fmt.Errorf("customers %q and %q produce the same session id - change one of their passwords", c.Name, other)
		}
		seenSessIDs[c.sessID] = c.Name
	}

	return list, nil
}
