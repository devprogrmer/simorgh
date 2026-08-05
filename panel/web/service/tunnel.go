package service

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/logger"
)

// The Simorgh tunnel, driven from the panel as one more core.
//
// It is a separate program (../core), and the obvious way to run it would be the
// Docker image install.sh builds. That was rejected: the panel currently runs
// fine on a host with no Docker, and adding a hard dependency on it to gain one
// feature would cost every operator who does not have it. The tunnel is pure Go
// with no third-party modules and reads its whole configuration from the
// environment, so it runs perfectly well as a plain supervised process -- which
// is exactly how procmgr already runs openvpn, xl2tpd, pptpd and the rest.
//
// So the tunnel becomes a core beside the others: same status vocabulary, same
// start/stop/restart, same log viewer, and the operator learns nothing new.

const tunnelProcName = "simorgh-tunnel"

// tunnelSettingKey holds the whole configuration as one JSON blob in the
// settings table.
//
// One key rather than twenty, because these values are only ever read and
// written together, and twenty rows would mean twenty chances for a partial
// write to leave a tunnel configured half one way and half another.
const tunnelSettingKey = "simorghTunnel"

// TunnelConfig is what an operator sets. The field names match the environment
// variables the core reads (docs/CONFIGURATION.md), so there is no translation
// table to fall out of step with the program it configures.
type TunnelConfig struct {
	Enable bool `json:"enable"`

	// Mode is "tun" (the tunnel IS the VPN) or "forward" (it only carries
	// someone else's protocol). OperatingMode is "client" on the Iran side and
	// "server" abroad.
	Mode          string `json:"mode"`
	OperatingMode string `json:"operatingMode"`

	// RemoteIP may be a comma-separated list on the client side. The core
	// measures RTT and loss on each and moves to whichever is genuinely best, so
	// this is how multi-server failover is configured -- there is no separate
	// switch for it.
	RemoteIP  string `json:"remoteIp"`
	Password  string `json:"-"` // never returned by the API; see MaskedConfig
	Transport string `json:"transport"`

	UDPPort int `json:"udpPort"`
	MTU     int `json:"mtu"`

	FECEnable bool `json:"fecEnable"`

	// Forward-mode only.
	ForwardProto string `json:"forwardProto"`
	ForwardBind  string `json:"forwardBind"`
	TargetHost   string `json:"targetHost"`
	TargetPort   int    `json:"targetPort"`
}

type TunnelService struct{}

// GetConfig reads the stored configuration, or sensible defaults on a host that
// has never configured one.
func (s *TunnelService) GetConfig() TunnelConfig {
	cfg := TunnelConfig{
		Mode:          "tun",
		OperatingMode: "client",
		Transport:     "icmp", // lowest overhead and least likely to be filtered
		UDPPort:       8443,
		MTU:           1400,
		FECEnable:     true, // recovers a lost packet without waiting for a retransmit
	}
	var setting SettingService
	raw, err := setting.getString(tunnelSettingKey)
	if err != nil || strings.TrimSpace(raw) == "" {
		return cfg
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		logger.Warning("tunnel: stored config is unreadable, using defaults:", err)
	}
	return cfg
}

// MaskedConfig is what the API returns: everything except the password.
//
// The password authenticates the handshake and is never the encryption key
// directly, but it is still the one secret that lets a stranger complete one --
// so it goes out masked, the same as every other credential in this panel.
func (s *TunnelService) MaskedConfig() map[string]any {
	cfg := s.GetConfig()
	b, _ := json.Marshal(cfg)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	m["passwordSet"] = s.storedPassword() != ""
	return m
}

func (s *TunnelService) storedPassword() string {
	var setting SettingService
	raw, err := setting.getString(tunnelSettingKey)
	if err != nil {
		return ""
	}
	var probe struct {
		Password string `json:"password"`
	}
	_ = json.Unmarshal([]byte(raw), &probe)
	return probe.Password
}

// SaveConfig validates and stores.
//
// An empty password keeps the stored one, so an operator editing the MTU does
// not have to retype a secret the form never showed them -- and cannot blank it
// by accident.
func (s *TunnelService) SaveConfig(cfg TunnelConfig) error {
	if err := validateTunnel(cfg); err != nil {
		return err
	}
	if cfg.Password == "" {
		cfg.Password = s.storedPassword()
	}
	if cfg.Enable && cfg.Password == "" {
		return fmt.Errorf("the tunnel needs a password; it is what authenticates the handshake on both sides")
	}

	// Stored with the password included, which is why the struct tag hides it
	// from the API rather than from JSON entirely.
	type stored struct {
		TunnelConfig
		Password string `json:"password"`
	}
	b, err := json.Marshal(stored{TunnelConfig: cfg, Password: cfg.Password})
	if err != nil {
		return err
	}
	var setting SettingService
	return setting.setString(tunnelSettingKey, string(b))
}

func validateTunnel(cfg TunnelConfig) error {
	switch cfg.Mode {
	case "tun", "forward":
	default:
		return fmt.Errorf("mode must be tun or forward, got %q", cfg.Mode)
	}
	switch cfg.OperatingMode {
	case "client", "server":
	default:
		return fmt.Errorf("operating mode must be client (Iran) or server (abroad), got %q", cfg.OperatingMode)
	}
	switch cfg.Transport {
	case "icmp", "udp", "auto":
	default:
		return fmt.Errorf("transport must be icmp, udp or auto, got %q", cfg.Transport)
	}
	// auto is a CLIENT strategy -- it tries icmp then udp and locks onto whichever
	// reaches the server. A server has nothing to try, so accepting it there would
	// silently configure something that cannot work.
	if cfg.Transport == "auto" && cfg.OperatingMode != "client" {
		return fmt.Errorf("transport auto only applies to the client (Iran) side; the server must be told icmp or udp")
	}
	if cfg.OperatingMode == "client" && strings.TrimSpace(cfg.RemoteIP) == "" {
		return fmt.Errorf("the client side needs at least one remote IP to reach")
	}
	for _, host := range strings.Split(cfg.RemoteIP, ",") {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		if net.ParseIP(host) == nil {
			return fmt.Errorf("%q is not an IP address; the remote list takes addresses, comma separated", host)
		}
	}
	if cfg.MTU != 0 && (cfg.MTU < 576 || cfg.MTU > 9000) {
		return fmt.Errorf("MTU %d is outside the usable range (576-9000)", cfg.MTU)
	}
	if cfg.UDPPort != 0 && (cfg.UDPPort < 1 || cfg.UDPPort > 65535) {
		return fmt.Errorf("UDP port %d is not a port", cfg.UDPPort)
	}
	if cfg.Mode == "forward" {
		if strings.TrimSpace(cfg.TargetHost) == "" || cfg.TargetPort == 0 {
			return fmt.Errorf("forward mode needs a target host and port to relay to")
		}
	}
	return nil
}

// tunnelEnv renders the configuration as the environment the core reads.
func (s *TunnelService) tunnelEnv(cfg TunnelConfig, password string) []string {
	env := []string{
		"MODE=" + cfg.Mode,
		"OPERATING_MODE=" + cfg.OperatingMode,
		"PASSWORD=" + password,
		"TRANSPORT=" + cfg.Transport,
	}
	if cfg.RemoteIP != "" {
		env = append(env, "REMOTE_IP="+cfg.RemoteIP)
	}
	if cfg.UDPPort != 0 {
		env = append(env, "UDP_PORT="+strconv.Itoa(cfg.UDPPort))
	}
	if cfg.MTU != 0 {
		env = append(env, "MTU="+strconv.Itoa(cfg.MTU))
	}
	if cfg.FECEnable {
		env = append(env, "FEC_ENABLE=1")
	}
	if cfg.Mode == "forward" {
		if cfg.ForwardProto != "" {
			env = append(env, "FORWARD_PROTO="+cfg.ForwardProto)
		}
		if cfg.ForwardBind != "" {
			env = append(env, "FORWARD_BIND="+cfg.ForwardBind)
		}
		env = append(env, "TARGET_HOST="+cfg.TargetHost,
			"TARGET_PORT="+strconv.Itoa(cfg.TargetPort))
	}
	return env
}

// Available reports whether the tunnel binary is on this host.
func (s *TunnelService) Available() bool {
	return daemonBin(tunnelProcName) != tunnelProcName
}

// Start brings the tunnel up under procmgr, which restarts it if it exits and
// stops it with the panel -- the same supervision every other daemon gets.
func (s *TunnelService) Start() error {
	cfg := s.GetConfig()
	if !cfg.Enable {
		return fmt.Errorf("the tunnel is not enabled")
	}
	if !s.Available() {
		return fmt.Errorf("the %s binary is not installed on this host", tunnelProcName)
	}
	password := s.storedPassword()
	if password == "" {
		return fmt.Errorf("the tunnel has no password configured")
	}
	return procMgr.Start(tunnelProcName, daemonBin(tunnelProcName), nil,
		s.tunnelEnv(cfg, password), "")
}

func (s *TunnelService) Stop() error { return procMgr.Stop(tunnelProcName) }

func (s *TunnelService) Restart() error {
	_ = s.Stop()
	return s.Start()
}

func (s *TunnelService) Logs() string { return procMgr.Logs(tunnelProcName) }

// IsRunning reports whether the tunnel process is up.
func (s *TunnelService) IsRunning() bool { return procMgr.IsRunning(tunnelProcName) }
