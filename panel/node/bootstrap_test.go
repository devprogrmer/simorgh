package node

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeSSH records what bootstrap would do to a machine, so the sequencing and
// the file contents can be checked without one.
type fakeSSH struct {
	ran     []string
	written map[string][]byte
	modes   map[string]string
	uname   string
	osrel   string
	failOn  string
}

func newFakeSSH() *fakeSSH {
	return &fakeSSH{
		written: map[string][]byte{},
		modes:   map[string]string{},
		uname:   "x86_64\n",
		osrel:   "NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.3 LTS\"\nID=ubuntu\n",
	}
}

func (f *fakeSSH) Run(cmd string) (string, error) {
	f.ran = append(f.ran, cmd)
	if f.failOn != "" && strings.Contains(cmd, f.failOn) {
		return "", errors.New("command failed")
	}
	switch {
	case strings.Contains(cmd, "uname -m"):
		return f.uname, nil
	case strings.Contains(cmd, "os-release"):
		return f.osrel, nil
	}
	return "", nil
}

func (f *fakeSSH) Write(path string, content []byte, mode string) error {
	f.written[path] = content
	f.modes[path] = mode
	return nil
}

func (f *fakeSSH) Close() error { return nil }

func TestNormaliseArch(t *testing.T) {
	for uname, want := range map[string]string{
		"x86_64":  "amd64",
		"amd64":   "amd64",
		"aarch64": "arm64",
		"arm64":   "arm64",
		"armv7l":  "arm",
		"mips64":  "", // not built for; must be refused rather than guessed
		"":        "",
	} {
		if got := normaliseArch(uname); got != want {
			t.Errorf("normaliseArch(%q) = %q; want %q", uname, got, want)
		}
	}
}

// Distributions quote PRETTY_NAME differently, and some omit it. Parsed from
// captured strings, so this needs no host.
func TestOSReleasePretty(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"double quoted": {`PRETTY_NAME="Ubuntu 24.04.3 LTS"`, "Ubuntu 24.04.3 LTS"},
		"single quoted": {`PRETTY_NAME='Debian GNU/Linux 12 (bookworm)'`, "Debian GNU/Linux 12 (bookworm)"},
		"unquoted":      {`PRETTY_NAME=Alpine`, "Alpine"},
		"absent":        {"NAME=Whatever\nID=whatever", ""},
		"with others":   {"ID=fedora\nPRETTY_NAME=\"Fedora Linux 43\"\nVERSION_ID=43", "Fedora Linux 43"},
	} {
		if got := osReleasePretty(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

// An architecture this project does not build for must stop bootstrap, not be
// guessed at. Sending an amd64 binary to a mips box produces "Exec format
// error" at service start, which reads like a corrupt upload and sends the
// operator looking in the wrong place entirely.
func TestDetectHostRefusesUnknownArchitecture(t *testing.T) {
	f := newFakeSSH()
	f.uname = "mips64\n"
	if _, _, err := detectHost(f); err == nil {
		t.Fatal("an unsupported architecture was accepted")
	} else if !strings.Contains(err.Error(), "mips64") {
		t.Errorf("the error should name what was found; got %v", err)
	}
}

// PEM is newline-rich, so the node config is only valid JSON if the blocks are
// escaped. A node that cannot parse its own config refuses to start, with the
// operator holding a "bootstrap succeeded" message.
func TestNodeConfigIsValidJSONWithPEM(t *testing.T) {
	f := newFakeSSH()
	caCert, caKey, err := GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := IssueNodeCert(caCert, caKey, "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	if err := installNode(f, []byte("BINARY"), caCert, cert, key, 62050); err != nil {
		t.Fatal(err)
	}

	raw, ok := f.written[remoteConfig]
	if !ok {
		t.Fatal("no node config was written")
	}
	var cfg struct {
		Listen string `json:"listen"`
		CACert string `json:"caCert"`
		Cert   string `json:"cert"`
		Key    string `json:"key"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("the node config is not valid JSON: %v\n%s", err, raw)
	}
	if cfg.Listen != "0.0.0.0:62050" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if !strings.Contains(cfg.CACert, "BEGIN CERTIFICATE") ||
		!strings.Contains(cfg.Key, "PRIVATE KEY") {
		t.Error("PEM blocks did not survive the round trip")
	}
	// The node's private key is in this file.
	if f.modes[remoteConfig] != "0600" {
		t.Errorf("node config mode = %q; want 0600", f.modes[remoteConfig])
	}
	if f.modes[remoteBinary] != "0755" {
		t.Errorf("binary mode = %q; want 0755", f.modes[remoteBinary])
	}
}

// systemd supervises the node PROCESS; the process supervises the VPN daemons
// itself, as procmgr.go does on the master. A unit per daemon would recreate the
// design that was deliberately migrated away from.
func TestUnitSupervisesOnlyTheNodeProcess(t *testing.T) {
	u := nodeUnit()
	if !strings.Contains(u, remoteBinary+" node --config "+remoteConfig) {
		t.Errorf("unit does not start the node in node mode:\n%s", u)
	}
	for _, daemon := range []string{"xl2tpd", "pptpd", "openvpn", "xray"} {
		if strings.Contains(u, daemon) {
			t.Errorf("unit mentions %s; the node process supervises daemons itself", daemon)
		}
	}
}

// The whole point of bootstrapping over SSH and then stopping: nothing the
// master keeps may carry the credential. Asserted by marshalling the result,
// which is what would reach the database and the API.
func TestBootstrapResultCarriesNoCredential(t *testing.T) {
	const password = "hunter2-the-root-password"
	const privKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nSECRET\n-----END OPENSSH PRIVATE KEY-----"

	res := BootstrapResult{
		Arch: "amd64", Distro: "Ubuntu 24.04", APIPort: 62050,
		ServerCert: "-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----",
		ServerKey:  "-----BEGIN EC PRIVATE KEY-----\nY\n-----END EC PRIVATE KEY-----",
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	for _, forbidden := range []string{password, privKey, "PRIVATE KEY", "BEGIN CERTIFICATE"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("BootstrapResult JSON leaks %q:\n%s", forbidden, body)
		}
	}
	// The request type holds them only for the length of the call, and is never
	// marshalled anywhere -- but if that ever changes, this is the reminder.
	req := BootstrapRequest{Host: "203.0.113.10", User: "root", Password: password}
	if req.Password == "" {
		t.Fatal("setup error")
	}
}

// A failure part-way through must surface which step failed. "Bootstrap failed"
// with no cause leaves an operator guessing between the network, the
// credentials, sshd and systemd.
func TestInstallNodeReportsTheFailingStep(t *testing.T) {
	f := newFakeSSH()
	f.failOn = "systemctl"
	caCert, caKey, _ := GenerateCA()
	cert, key, _ := IssueNodeCert(caCert, caKey, "203.0.113.10")

	err := installNode(f, []byte("BINARY"), caCert, cert, key, 62050)
	if err == nil {
		t.Fatal("a failing systemctl was reported as success")
	}
	if !strings.Contains(err.Error(), "starting the node service") {
		t.Errorf("the error should name the step; got %v", err)
	}
}

// Neither a password nor a key is a configuration mistake, and must be refused
// before anything is dialled.
func TestBootstrapRequiresCredentials(t *testing.T) {
	_, err := Bootstrap(context.Background(),
		BootstrapRequest{Host: "203.0.113.10", User: "root"}, nil, nil, nil)
	if err == nil {
		t.Fatal("bootstrap proceeded with no credentials")
	}
}
