package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds every runtime knob, sourced from environment variables set by
// the Simorgh manager script (install.sh -> start_logic()).
type Config struct {
	Interface string // TUN/TAP name, e.g. "simorgh0"
	Password  string // pre-shared secret, used only to authenticate the handshake
	IsClient  bool   // true = IRAN/client role, false = FOREIGN/server role

	Transport string // "icmp" (default) or "udp"
	RemoteIP  string // client only: the foreign server's public IP
	UDPPort   int    // used only when Transport == "udp"
	MAC       string // optional custom MAC for TAP mode

	KeepaliveSec  int
	LinkQuality   int
	BanQuality    int
	MTU           int
	DSCPMark      int
	BandwidthMbps float64
	OperatingMode string

	FECEnable bool
	FECGroup  int // number of data packets per parity packet

	// Mode: "tun" (default - full L3 tunnel, own TUN device) or "forward"
	// (relay another VPN's already-encrypted UDP traffic - e.g. WireGuard
	// or OpenVPN - through Simorgh as a low-overhead, ICMP-carried
	// transport. Simorgh does not replace that VPN's encryption in this
	// mode; it only carries its packets.)
	Mode string

	// forward mode, client (IRAN) side: local UDP listener your
	// WireGuard/OpenVPN client points its Endpoint at.
	ForwardBind  string
	LocalPort    int
	ForwardProto string // "udp" (default) or "tcp" - must match the real VPN's own transport

	// forward mode, server (FOREIGN) side: the real WireGuard/OpenVPN
	// server this relays to, normally on localhost.
	TargetHost string
	TargetPort int

	// Multi-server auto-failover (client only). RemoteIP may be a single
	// host or a comma-separated list; with more than one, the client
	// continuously measures real RTT+loss to each and switches to
	// whichever currently scores best.
	HealthCheckSec      int // how often to (re-)probe every candidate server
	SwitchMarginMs      int // a candidate must beat the active path's score by at least this much
	SwitchConfirmRounds int // ...for this many consecutive checks before switching
	PingIntervalSec     int // how often to ping the currently active session for live RTT/loss

	// Multi-client server support (server + forward mode only). When set,
	// the server loads a {name,password} list from this JSON file and
	// serves each one as a fully isolated session - its own crypto, FEC,
	// quality tracking, and relay socket to TargetHost:TargetPort - instead
	// of the single global PASSWORD. See customers.go.
	CustomersFile string
}

func loadConfig() (*Config, error) {
	c := &Config{
		Interface:           getenv("INTERFACE", "simorgh0"),
		Password:            os.Getenv("PASSWORD"),
		Transport:           strings.ToLower(getenv("TRANSPORT", "icmp")),
		UDPPort:             getenvInt("UDP_PORT", 51900),
		KeepaliveSec:        getenvInt("KEEPALIVE", 5),
		MTU:                 getenvInt("MTU", 1400),
		MAC:                 os.Getenv("MAC"),
		LinkQuality:         getenvInt("LINK_QUALITY", 0),
		BanQuality:          getenvInt("BAN_QUALITY", 0),
		DSCPMark:            getenvInt("DSCP_MARK", -1),
		OperatingMode:       os.Getenv("OPERATING_MODE"),
		FECEnable:           getenvBool("FEC_ENABLE", false),
		FECGroup:            getenvInt("FEC_GROUP", 8),
		Mode:                strings.ToLower(getenv("MODE", "tun")),
		ForwardBind:         getenv("FORWARD_BIND", "127.0.0.1"),
		LocalPort:           getenvInt("LOCAL_PORT", 51000),
		ForwardProto:        strings.ToLower(getenv("FORWARD_PROTO", "udp")),
		TargetHost:          getenv("TARGET_HOST", "127.0.0.1"),
		TargetPort:          getenvInt("TARGET_PORT", 51820),
		HealthCheckSec:      getenvInt("HEALTH_CHECK_INTERVAL", 5),
		SwitchMarginMs:      getenvInt("SWITCH_MARGIN_MS", 15),
		SwitchConfirmRounds: getenvInt("SWITCH_CONFIRM_ROUNDS", 3),
		PingIntervalSec:     getenvInt("PING_INTERVAL", 1),
		CustomersFile:       os.Getenv("CUSTOMERS_FILE"),
	}

	if remote := os.Getenv("REMOTE_IP"); remote != "" {
		c.IsClient = true
		c.RemoteIP = remote
	} else {
		c.IsClient = false
	}

	if c.Password == "" && (c.IsClient || c.CustomersFile == "") {
		return nil, fmt.Errorf("PASSWORD is required (unless CUSTOMERS_FILE is set on the server)")
	}
	if c.IsClient && c.CustomersFile != "" {
		return nil, fmt.Errorf("CUSTOMERS_FILE is a server-only setting; the client always uses a single PASSWORD")
	}
	if c.Transport != "icmp" && c.Transport != "udp" && c.Transport != "auto" {
		return nil, fmt.Errorf("TRANSPORT must be 'icmp', 'udp', or 'auto', got %q", c.Transport)
	}
	if c.Transport == "auto" && !c.IsClient {
		return nil, fmt.Errorf("TRANSPORT=auto is client-only; the server must pick a concrete transport (see docs/DEPLOYMENT.md for running both)")
	}
	if c.Mode != "tun" && c.Mode != "forward" {
		return nil, fmt.Errorf("MODE must be 'tun' or 'forward', got %q", c.Mode)
	}
	if c.ForwardProto != "udp" && c.ForwardProto != "tcp" {
		return nil, fmt.Errorf("FORWARD_PROTO must be 'udp' or 'tcp', got %q", c.ForwardProto)
	}
	if !c.IsClient && c.CustomersFile != "" && c.Mode != "forward" {
		return nil, fmt.Errorf("CUSTOMERS_FILE (multi-client) is currently only supported with MODE=forward")
	}

	if bw := os.Getenv("BANDWIDTH_LIMIT"); bw != "" {
		if v, err := strconv.ParseFloat(strings.TrimSpace(bw), 64); err == nil {
			c.BandwidthMbps = v
		}
	}

	if c.KeepaliveSec <= 0 {
		c.KeepaliveSec = 5
	}
	if c.MTU <= 0 {
		c.MTU = 1400
	}
	if c.FECGroup <= 1 {
		c.FECGroup = 8
	}
	if c.HealthCheckSec <= 0 {
		c.HealthCheckSec = 5
	}
	if c.SwitchConfirmRounds <= 0 {
		c.SwitchConfirmRounds = 3
	}
	if c.PingIntervalSec <= 0 {
		c.PingIntervalSec = 1
	}

	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func getenvBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
