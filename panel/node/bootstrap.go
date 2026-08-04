package node

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Bootstrapping a node: SSH once, then never again.
//
// SSH exists here only to plant the binary and the certificate. Once the mTLS
// channel is up it is never used again and the credentials are discarded --
// they are never written to the database, and BootstrapResult carries none of
// them. Keeping a root password for every node would make the panel's database
// a list of root logins for the whole fleet.

// BootstrapRequest is what an operator supplies when adding a node.
type BootstrapRequest struct {
	Host string
	Port int    // SSH port; 0 means 22
	User string // usually root
	// Exactly one of these. Both are held in memory for the length of the call
	// and never persisted.
	Password   string
	PrivateKey string

	APIPort int // the port the node will serve its API on; 0 means 62050
}

// BootstrapResult is what the master keeps. Deliberately carries no credential:
// anything in this struct may end up in the database.
type BootstrapResult struct {
	Arch       string `json:"arch"`
	Distro     string `json:"distro"`
	ServerCert string `json:"-"`
	ServerKey  string `json:"-"`
	APIPort    int    `json:"apiPort"`
}

const (
	defaultSSHPort  = 22
	defaultAPIPort  = 62050
	remoteBinary    = "/usr/local/bin/simorgh-panel"
	remoteConfig    = "/etc/simorgh-node.json"
	remoteUnit      = "/etc/systemd/system/simorgh-node.service"
	bootstrapDialTO = 30 * time.Second
)

// sshRunner is the small slice of an SSH connection bootstrap needs, so the
// sequencing can be tested without a server.
type sshRunner interface {
	Run(cmd string) (string, error)
	Write(path string, content []byte, mode string) error
	Close() error
}

// Bootstrap prepares a remote machine and returns what the master should store.
//
// The certificate is issued here rather than on the node so the node never holds
// the CA key: a compromised node can then impersonate only itself, not mint
// credentials for the rest of the fleet.
func Bootstrap(ctx context.Context, req BootstrapRequest, caCert, caKey []byte, binary []byte) (BootstrapResult, error) {
	var res BootstrapResult
	if req.APIPort == 0 {
		req.APIPort = defaultAPIPort
	}
	res.APIPort = req.APIPort

	conn, err := dialSSH(req)
	if err != nil {
		// The underlying error is returned verbatim. "Could not connect" tells an
		// operator nothing; "no supported methods remain" and "connection refused"
		// send them to different places.
		return res, fmt.Errorf("ssh %s: %w", req.Host, err)
	}
	defer conn.Close()

	res.Arch, res.Distro, err = detectHost(conn)
	if err != nil {
		return res, err
	}

	certPEM, keyPEM, err := IssueNodeCert(caCert, caKey, req.Host)
	if err != nil {
		return res, fmt.Errorf("issuing the node certificate: %w", err)
	}
	res.ServerCert, res.ServerKey = string(certPEM), string(keyPEM)

	if err := installNode(conn, binary, caCert, certPEM, keyPEM, req.APIPort); err != nil {
		return res, err
	}
	return res, nil
}

// detectHost reads the architecture and distribution.
//
// The architecture decides which binary to send; getting it wrong produces an
// "Exec format error" at service start, which reads like a corrupt upload.
func detectHost(conn sshRunner) (arch, distro string, err error) {
	out, err := conn.Run("uname -m")
	if err != nil {
		return "", "", fmt.Errorf("reading the architecture: %w", err)
	}
	arch = normaliseArch(strings.TrimSpace(out))
	if arch == "" {
		return "", "", fmt.Errorf("unsupported architecture %q", strings.TrimSpace(out))
	}
	rel, err := conn.Run("cat /etc/os-release 2>/dev/null || true")
	if err == nil {
		distro = osReleasePretty(rel)
	}
	return arch, distro, nil
}

// normaliseArch maps uname output onto Go's GOARCH names, and returns "" for
// anything this project does not build for.
func normaliseArch(uname string) string {
	switch uname {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armv6l":
		return "arm"
	default:
		return ""
	}
}

// osReleasePretty pulls PRETTY_NAME out of /etc/os-release, tolerating the
// quoting variations different distributions use.
func osReleasePretty(osRelease string) string {
	for _, line := range strings.Split(osRelease, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "PRETTY_NAME=") {
			continue
		}
		v := strings.TrimPrefix(line, "PRETTY_NAME=")
		v = strings.Trim(v, `"'`)
		return v
	}
	return ""
}

// nodeUnit is the systemd unit for the node PROCESS only.
//
// systemd supervises this one process; the process supervises the VPN daemons
// itself as children, the same way the panel does (see procmgr.go). Generating a
// unit per daemon would recreate the design that was deliberately migrated away
// from, and would leave orphans when the node is removed.
func nodeUnit() string {
	return `[Unit]
Description=Simorgh node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=` + remoteBinary + ` node --config ` + remoteConfig + `
Restart=on-failure
RestartSec=5
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
`
}

func installNode(conn sshRunner, binary, caCert, cert, key []byte, apiPort int) error {
	if len(binary) > 0 {
		if err := conn.Write(remoteBinary, binary, "0755"); err != nil {
			return fmt.Errorf("uploading the node binary: %w", err)
		}
	}

	cfg := fmt.Sprintf(`{"listen":"0.0.0.0:%d","caCert":%s,"cert":%s,"key":%s}`,
		apiPort, jsonString(string(caCert)), jsonString(string(cert)), jsonString(string(key)))
	// 0600: the node's private key is in here.
	if err := conn.Write(remoteConfig, []byte(cfg), "0600"); err != nil {
		return fmt.Errorf("writing the node config: %w", err)
	}
	if err := conn.Write(remoteUnit, []byte(nodeUnit()), "0644"); err != nil {
		return fmt.Errorf("writing the systemd unit: %w", err)
	}
	if _, err := conn.Run("systemctl daemon-reload && systemctl enable --now simorgh-node"); err != nil {
		return fmt.Errorf("starting the node service: %w", err)
	}
	return nil
}

// jsonString quotes a PEM block for embedding in the config. PEM is newline-rich
// and would otherwise produce invalid JSON.
func jsonString(s string) string {
	var b bytes.Buffer
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// --------------------------------------------------------------------------- //
//  real SSH
// --------------------------------------------------------------------------- //

type sshConn struct{ client *ssh.Client }

func dialSSH(req BootstrapRequest) (sshRunner, error) {
	port := req.Port
	if port == 0 {
		port = defaultSSHPort
	}
	var auth []ssh.AuthMethod
	switch {
	case req.PrivateKey != "":
		signer, err := ssh.ParsePrivateKey([]byte(req.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("the private key could not be parsed: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	case req.Password != "":
		auth = append(auth, ssh.Password(req.Password))
	default:
		return nil, fmt.Errorf("no password or private key was supplied")
	}

	cfg := &ssh.ClientConfig{
		User: req.User,
		Auth: auth,
		// The host key is not known in advance: this is the operator adding a
		// machine they already control, by an address they typed. Pinning would
		// require them to paste a fingerprint they have no way to obtain yet.
		// The window is one connection long -- everything after this is mTLS
		// against a CA the master itself issued.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         bootstrapDialTO,
	}
	client, err := ssh.Dial("tcp", net.JoinHostPort(req.Host, strconv.Itoa(port)), cfg)
	if err != nil {
		return nil, err
	}
	return &sshConn{client: client}, nil
}

func (c *sshConn) Run(cmd string) (string, error) {
	sess, err := c.client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdout = &out
	sess.Stderr = &errb
	if err := sess.Run(cmd); err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Write streams content to a remote path via `cat`, then chmods it.
//
// cat rather than SFTP because the SFTP subsystem is not enabled everywhere and
// a node that could not be added because of sshd config would be a confusing
// failure for something this project can do with the shell it already has.
func (c *sshConn) Write(path string, content []byte, mode string) error {
	sess, err := c.client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	stdin, err := sess.StdinPipe()
	if err != nil {
		return err
	}
	// Written to a temp path and moved into place, so a half-uploaded binary is
	// never what systemd tries to execute.
	tmp := path + ".partial"
	cmd := fmt.Sprintf("cat > %s && chmod %s %s && mv -f %s %s", tmp, mode, tmp, tmp, path)
	if err := sess.Start(cmd); err != nil {
		return err
	}
	if _, err := io.Copy(stdin, bytes.NewReader(content)); err != nil {
		return err
	}
	if err := stdin.Close(); err != nil {
		return err
	}
	return sess.Wait()
}

func (c *sshConn) Close() error { return c.client.Close() }
