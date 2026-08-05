package service

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database"
)

func tunnelDB(t *testing.T) *TunnelService {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	return &TunnelService{}
}

func validClient() TunnelConfig {
	return TunnelConfig{
		Enable: true, Mode: "tun", OperatingMode: "client",
		RemoteIP: "203.0.113.10", Password: "secret", Transport: "icmp",
		UDPPort: 8443, MTU: 1400, FECEnable: true,
	}
}

// A host that has never configured a tunnel gets defaults that are the right
// starting point rather than zeroes: icmp because it has the lowest overhead and
// is least likely to be filtered, and FEC on because it recovers a lost packet
// without waiting for a retransmit, which is the latency spike that hurts gaming
// most.
func TestTunnelDefaultsAreUsable(t *testing.T) {
	cfg := tunnelDB(t).GetConfig()
	if cfg.Transport != "icmp" {
		t.Errorf("default transport = %q; want icmp", cfg.Transport)
	}
	if !cfg.FECEnable {
		t.Error("FEC should default on")
	}
	if cfg.MTU == 0 || cfg.UDPPort == 0 {
		t.Errorf("defaults leave MTU/port unset: %+v", cfg)
	}
	if cfg.Enable {
		t.Error("a tunnel must not default to enabled on a host that never configured one")
	}
}

// transport=auto is a CLIENT strategy: it tries icmp, then udp, and locks onto
// whichever reaches the server. A server has nothing to try, so accepting it
// there would silently configure something that cannot work and leave the
// operator debugging the network.
func TestAutoTransportIsRefusedOnTheServerSide(t *testing.T) {
	s := tunnelDB(t)
	cfg := validClient()
	cfg.OperatingMode = "server"
	cfg.Transport = "auto"
	cfg.RemoteIP = ""

	err := s.SaveConfig(cfg)
	if err == nil {
		t.Fatal("transport auto was accepted on the server side")
	}
	if !strings.Contains(err.Error(), "client") {
		t.Errorf("the error should say which side auto applies to; got %v", err)
	}
}

// The client needs somewhere to go.
func TestClientNeedsARemote(t *testing.T) {
	s := tunnelDB(t)
	cfg := validClient()
	cfg.RemoteIP = ""
	if err := s.SaveConfig(cfg); err == nil {
		t.Fatal("a client with no remote IP was accepted")
	}
}

// The remote list takes addresses. A hostname here fails at runtime inside the
// core with an error the panel never sees, so it is refused at the form.
func TestRemoteListMustBeAddresses(t *testing.T) {
	s := tunnelDB(t)
	cfg := validClient()
	cfg.RemoteIP = "203.0.113.10, not-an-ip"
	err := s.SaveConfig(cfg)
	if err == nil {
		t.Fatal("a non-address in the remote list was accepted")
	}
	if !strings.Contains(err.Error(), "not-an-ip") {
		t.Errorf("the error should name the bad entry; got %v", err)
	}
}

// Several remotes is the multi-server failover configuration -- the core
// measures each and moves to whichever is best -- so a comma-separated list must
// be accepted.
func TestMultipleRemotesAreAccepted(t *testing.T) {
	s := tunnelDB(t)
	cfg := validClient()
	cfg.RemoteIP = "203.0.113.10, 198.51.100.7,192.0.2.44"
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatalf("a multi-server client was refused: %v", err)
	}
}

// Forward mode carries someone else's protocol, so it needs to know where to
// send it.
func TestForwardModeNeedsATarget(t *testing.T) {
	s := tunnelDB(t)
	cfg := validClient()
	cfg.Mode = "forward"
	if err := s.SaveConfig(cfg); err == nil {
		t.Fatal("forward mode was accepted with no target")
	}
	cfg.TargetHost = "10.0.0.5"
	cfg.TargetPort = 51820
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatalf("a complete forward config was refused: %v", err)
	}
}

// Editing the MTU must not blank the password the form never showed.
func TestEmptyPasswordKeepsTheStoredOne(t *testing.T) {
	s := tunnelDB(t)
	if err := s.SaveConfig(validClient()); err != nil {
		t.Fatal(err)
	}
	edit := validClient()
	edit.Password = "" // as the form posts it back
	edit.MTU = 1380
	if err := s.SaveConfig(edit); err != nil {
		t.Fatal(err)
	}
	if got := s.storedPassword(); got != "secret" {
		t.Fatalf("password after an unrelated edit = %q; want it preserved", got)
	}
	if s.GetConfig().MTU != 1380 {
		t.Error("the edit did not take")
	}
}

// Enabling a tunnel with no password anywhere is refused: the password is what
// authenticates the handshake, and a tunnel without one cannot come up.
func TestEnablingWithoutAPasswordIsRefused(t *testing.T) {
	s := tunnelDB(t)
	cfg := validClient()
	cfg.Password = ""
	if err := s.SaveConfig(cfg); err == nil {
		t.Fatal("an enabled tunnel with no password was accepted")
	}
}

// The password must never reach the API, the same as every other credential in
// this panel.
func TestMaskedConfigHidesThePassword(t *testing.T) {
	s := tunnelDB(t)
	if err := s.SaveConfig(validClient()); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(s.MaskedConfig())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret") {
		t.Fatalf("the password reached the API payload:\n%s", b)
	}
	// But the UI still has to know whether one is set, or it cannot tell an
	// operator that enabling will fail.
	if !strings.Contains(string(b), "passwordSet") {
		t.Error("the API must report whether a password exists")
	}
}

// The environment handed to the core is what actually configures it, so the
// mapping is worth pinning: a renamed variable here is a setting that silently
// stops applying.
func TestEnvCarriesTheConfiguration(t *testing.T) {
	s := tunnelDB(t)
	cfg := validClient()
	cfg.Mode = "forward"
	cfg.TargetHost = "10.0.0.5"
	cfg.TargetPort = 51820
	cfg.ForwardProto = "udp"

	env := s.tunnelEnv(cfg, "secret")
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"MODE=forward", "OPERATING_MODE=client", "PASSWORD=secret",
		"TRANSPORT=icmp", "REMOTE_IP=203.0.113.10", "UDP_PORT=8443",
		"MTU=1400", "FEC_ENABLE=1", "FORWARD_PROTO=udp",
		"TARGET_HOST=10.0.0.5", "TARGET_PORT=51820",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("env is missing %q:\n%s", want, joined)
		}
	}
}

// Forward-only settings must not leak into a tun-mode tunnel, where the core
// would either ignore them or act on them unexpectedly.
func TestTunModeOmitsForwardSettings(t *testing.T) {
	s := tunnelDB(t)
	cfg := validClient() // Mode: tun
	cfg.TargetHost = "10.0.0.5"
	cfg.TargetPort = 51820

	joined := strings.Join(s.tunnelEnv(cfg, "secret"), "\n")
	if strings.Contains(joined, "TARGET_HOST") {
		t.Errorf("tun mode leaked forward settings into the environment:\n%s", joined)
	}
}

// An out-of-range MTU is refused at the form rather than producing a tunnel that
// silently drops every large packet.
func TestMTUIsRangeChecked(t *testing.T) {
	s := tunnelDB(t)
	for _, bad := range []int{100, 20000} {
		cfg := validClient()
		cfg.MTU = bad
		if err := s.SaveConfig(cfg); err == nil {
			t.Errorf("MTU %d was accepted", bad)
		}
	}
}
