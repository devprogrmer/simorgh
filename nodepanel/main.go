// Command simorgh-nodepanel is a small, standalone web UI for creating
// multi-location WireGuard customer configs without manual SSH. It is
// intentionally NOT a patch to panel/ (the vpn-ui fork) - that codebase is
// large, GPLv3, and untested in this build environment, so risky surgery
// on it was avoided. This is a separate, small, own tool instead: it
// remotely invokes the same protocols/wireguard.sh functions already
// tested in this project, over plain SSH, and shows you the result.
//
// A "node" here is just a registered SSH target (host, user, port) that
// has already had install.sh run on it (so protocols/wireguard.sh and
// wg_install_core are already in place there). Nothing new needs to be
// installed on a node beyond what install.sh already sets up.
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sync"
)

//go:embed embedded/wireguard.sh embedded/openvpn.sh
var embeddedFS embed.FS

type Node struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	SSHUser   string `json:"ssh_user"`
	SSHPort   int    `json:"ssh_port"`
	Protocol  string `json:"protocol"` // "wireguard" or "openvpn"
	OVPNPort  int    `json:"ovpn_port,omitempty"`
	OVPNProto string `json:"ovpn_proto,omitempty"`
}

type store struct {
	mu    sync.Mutex
	path  string
	Nodes []Node `json:"nodes"`
}

func newStore(path string) *store {
	s := &store{path: path}
	s.load()
	return s
}

func (s *store) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		return // fine - no file yet, starts empty
	}
	_ = json.Unmarshal(data, s)
}

func (s *store) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}

func (s *store) addNode(n Node) {
	s.mu.Lock()
	s.Nodes = append(s.Nodes, n)
	s.mu.Unlock()
}

func (s *store) removeNode(name string) {
	s.mu.Lock()
	out := s.Nodes[:0]
	for _, n := range s.Nodes {
		if n.Name != name {
			out = append(out, n)
		}
	}
	s.Nodes = out
	s.mu.Unlock()
}

func (s *store) find(name string) (Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return Node{}, false
}

func (s *store) firstOpenVPNNode() (Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.Nodes {
		if n.Protocol == "openvpn" {
			return n, true
		}
	}
	return Node{}, false
}

// runOnNode invokes wg_add_customer on the remote node over SSH, direct
// (non-carrier) mode: the returned config points straight at the node's
// public host. It relies entirely on protocols/wireguard.sh already being
// present there via install.sh - no new remote-side code.
func runOnNode(n Node, customerName, bwMbps string) (string, error) {
	remoteCmd := fmt.Sprintf(
		`source /usr/local/share/simorgh/protocols/wireguard.sh && WG_DIRECT_HOST=%s LOG_FILE=/tmp/simorgh_install.log wg_add_customer %s %s`,
		shellQuote(n.Host), shellQuote(customerName), shellQuote(bwMbps),
	)

	args := []string{
		"-i", nodepanelKeyPath,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-p", fmt.Sprintf("%d", n.SSHPort),
		fmt.Sprintf("%s@%s", n.SSHUser, n.Host),
		remoteCmd,
	}
	cmd := exec.Command("ssh", args...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ssh to %s failed: %w\n%s", n.Name, err, stderr.String())
	}
	return out.String(), nil
}

// shellQuote wraps a value in single quotes for safe inclusion in the
// remote command line, escaping any embedded single quotes.
func shellQuote(s string) string {
	return "'" + bytesReplaceAll(s, "'", `'"'"'`) + "'"
}

func bytesReplaceAll(s, old, new string) string {
	// tiny local helper so this file has zero non-stdlib dependencies
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if old != "" && i+len(old) <= len(s) && s[i:i+len(old)] == old {
			out = append(out, new...)
			i += len(old) - 1
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

// ---- password-based bootstrap: install key, push wireguard.sh, install WG ----

const nodepanelKeyPath = "/etc/simorgh/nodepanel_id_ed25519"

// ensureLocalKey makes sure we have our own SSH keypair to install onto
// nodes during bootstrap, so ordinary (non-bootstrap) operations never need
// the node's password again afterward.
func ensureLocalKey() (pubKeyPath string, err error) {
	pubKeyPath = nodepanelKeyPath + ".pub"
	if _, err := os.Stat(nodepanelKeyPath); err == nil {
		return pubKeyPath, nil
	}
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", nodepanelKeyPath, "-q")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("generate nodepanel ssh key: %w\n%s", err, stderr.String())
	}
	return pubKeyPath, nil
}

// installKeyOnNode is bootstrap step 1, shared by every protocol: use the
// node's password exactly once to install our own key, so nothing else
// ever needs that password again.
func installKeyOnNode(host string, port int, user, password string) error {
	pubKeyPath, err := ensureLocalKey()
	if err != nil {
		return err
	}
	pubKey, err := os.ReadFile(pubKeyPath)
	if err != nil {
		return fmt.Errorf("read local public key: %w", err)
	}
	sshTarget := fmt.Sprintf("%s@%s", user, host)
	installKeyCmd := fmt.Sprintf(
		`mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo %s >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys`,
		shellQuote(string(pubKey)),
	)
	if out, err := sshpassRun(password, port, sshTarget, installKeyCmd); err != nil {
		return fmt.Errorf("installing SSH key on node failed: %w\n%s", err, out)
	}
	return nil
}

// pushEmbeddedScript copies one of our embedded protocol scripts to a node
// over the (by now key-trusted) SSH connection.
func pushEmbeddedScript(host string, port int, user, embeddedName, remotePath string) error {
	scriptData, err := embeddedFS.ReadFile("embedded/" + embeddedName)
	if err != nil {
		return fmt.Errorf("read embedded %s: %w", embeddedName, err)
	}
	sshTarget := fmt.Sprintf("%s@%s", user, host)
	pushCmd := exec.Command("ssh",
		"-i", nodepanelKeyPath, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new",
		"-p", fmt.Sprintf("%d", port), sshTarget,
		fmt.Sprintf("mkdir -p %s && cat > %s", shellQuote(pathDir(remotePath)), remotePath),
	)
	pushCmd.Stdin = bytes.NewReader(scriptData)
	var pushErr bytes.Buffer
	pushCmd.Stderr = &pushErr
	if err := pushCmd.Run(); err != nil {
		return fmt.Errorf("pushing %s failed: %w\n%s", embeddedName, err, pushErr.String())
	}
	return nil
}

func pathDir(p string) string {
	i := bytes.LastIndexByte([]byte(p), '/')
	if i < 0 {
		return "."
	}
	return p[:i]
}

// runRemote runs one command on a node over the trusted key, capturing
// stdout+stderr together for display, and never fails hard on a non-zero
// exit - the caller decides what to show the admin either way.
func runRemote(host string, port int, user, remoteCmd string) (string, error) {
	sshTarget := fmt.Sprintf("%s@%s", user, host)
	cmd := exec.Command("ssh",
		"-i", nodepanelKeyPath, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new",
		"-p", fmt.Sprintf("%d", port), sshTarget, remoteCmd,
	)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := out.String()
	if stderr.Len() > 0 {
		result += "\n--- stderr ---\n" + stderr.String()
	}
	return result, err
}

const wgScriptPath = "/usr/local/share/simorgh/protocols/wireguard.sh"
const ovpnScriptPath = "/usr/local/share/simorgh/protocols/openvpn.sh"

// bootstrapWireGuardNode takes SSH host/port/user/password, installs our own
// public key on the node (so future operations never need the password
// again), pushes the embedded wireguard.sh, and runs wg_install_core there
// non-interactively. It never persists the password anywhere.
func bootstrapWireGuardNode(host string, port int, user, password, wgPort, wgSubnet string) (string, error) {
	if err := installKeyOnNode(host, port, user, password); err != nil {
		return "", err
	}
	if err := pushEmbeddedScript(host, port, user, "wireguard.sh", wgScriptPath); err != nil {
		return "", err
	}
	installCmd := fmt.Sprintf(
		`source %s && WG_LISTEN_PORT=%s WG_SERVER_SUBNET=%s LOG_FILE=/tmp/simorgh_install.log wg_install_core </dev/null`,
		wgScriptPath, shellQuote(wgPort), shellQuote(wgSubnet),
	)
	result, runErr := runRemote(host, port, user, installCmd)
	// capture but don't fail hard - wg-quick may legitimately not come up if
	// this node's own kernel lacks the WireGuard module; the admin needs to
	// see that output either way, not a silent failure.
	if runErr != nil {
		result += fmt.Sprintf("\n--- wg_install_core exited with an error: %v ---\n"+
			"The SSH key was still installed and wireguard.sh was still pushed - fix "+
			"whatever this error describes on the node directly, then re-run bootstrap "+
			"(it's safe to re-run).", runErr)
	}
	return result, nil
}

// bootstrapOpenVPNNode is the OpenVPN counterpart. If an OpenVPN node
// already exists in the store, its CA is exported and imported into the
// new node first, so one client certificate ends up valid on every
// OpenVPN node - that's what makes a single multi-remote .ovpn possible.
// The first OpenVPN node just generates its own fresh CA.
func bootstrapOpenVPNNode(s *store, host string, port int, user, password, ovpnPort, ovpnProto string) (string, error) {
	if err := installKeyOnNode(host, port, user, password); err != nil {
		return "", err
	}
	if err := pushEmbeddedScript(host, port, user, "openvpn.sh", ovpnScriptPath); err != nil {
		return "", err
	}

	var caImportPrefix string
	if existing, ok := s.firstOpenVPNNode(); ok {
		caTar, err := exportCAFromNode(existing)
		if err != nil {
			return "", fmt.Errorf("exporting CA from existing node %q failed: %w", existing.Name, err)
		}
		defer os.Remove(caTar)
		if err := pushCAToNode(host, port, user, caTar); err != nil {
			return "", fmt.Errorf("pushing shared CA to new node failed: %w", err)
		}
		caImportPrefix = `OVPN_SHARED_CA_DIR=/tmp/simorgh-shared-ca/easy-rsa `
	}

	installCmd := fmt.Sprintf(
		`source %s && %sOVPN_PORT=%s OVPN_PROTO=%s LOG_FILE=/tmp/simorgh_install.log ovpn_install_core </dev/null`,
		ovpnScriptPath, caImportPrefix, shellQuote(ovpnPort), shellQuote(ovpnProto),
	)
	result, runErr := runRemote(host, port, user, installCmd)
	if runErr != nil {
		result += fmt.Sprintf("\n--- ovpn_install_core exited with an error: %v ---\n"+
			"The SSH key was still installed and openvpn.sh was still pushed - fix "+
			"whatever this error describes on the node directly, then re-run bootstrap.", runErr)
	}
	return result, nil
}

// exportCAFromNode runs ovpn_export_ca_tarball on an existing node and
// pulls the resulting tarball back to a local temp file.
func exportCAFromNode(n Node) (string, error) {
	exportCmd := fmt.Sprintf(`source %s && ovpn_export_ca_tarball /tmp/simorgh-ca-export.tar.gz && cat /tmp/simorgh-ca-export.tar.gz`, ovpnScriptPath)
	sshTarget := fmt.Sprintf("%s@%s", n.SSHUser, n.Host)
	cmd := exec.Command("ssh",
		"-i", nodepanelKeyPath, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new",
		"-p", fmt.Sprintf("%d", n.SSHPort), sshTarget, exportCmd,
	)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w\n%s", err, stderr.String())
	}
	f, err := os.CreateTemp("", "simorgh-ca-*.tar.gz")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(out.Bytes()); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// pushCAToNode uploads and extracts a CA tarball on a (newly key-trusted) node.
func pushCAToNode(host string, port int, user, localTarPath string) error {
	data, err := os.ReadFile(localTarPath)
	if err != nil {
		return err
	}
	sshTarget := fmt.Sprintf("%s@%s", user, host)
	cmd := exec.Command("ssh",
		"-i", nodepanelKeyPath, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new",
		"-p", fmt.Sprintf("%d", port), sshTarget,
		"mkdir -p /tmp/simorgh-shared-ca && tar -xzf - -C /tmp/simorgh-shared-ca",
	)
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w\n%s", err, stderr.String())
	}
	return nil
}

// createOpenVPNCustomer generates ONE client certificate (on the first
// selected node - any of them works, since they share a CA) and combines
// it with a <connection> block per selected node into a single multi-remote
// .ovpn. If one location is down, OpenVPN itself fails over to the next
// <connection> block automatically - no orchestration needed on our side
// beyond generating this one file.
// issueSharedClientCert generates ONE client certificate on the given node
// (any node works - they all trust the same CA) and returns the four PEM
// blocks a .ovpn needs. Used by both output modes below.
func issueSharedClientCert(primary Node, customerName string) (ca, cert, key, ta string, err error) {
	remoteCmd := fmt.Sprintf(`source %s && OVPN_DIRECT_HOST=%s LOG_FILE=/tmp/simorgh_install.log ovpn_add_customer %s`,
		ovpnScriptPath, shellQuote(primary.Host), shellQuote(customerName))
	out, runErr := runRemote(primary.Host, primary.SSHPort, primary.SSHUser, remoteCmd)
	if runErr != nil {
		return "", "", "", "", fmt.Errorf("creating client cert on %s failed: %w\n%s", primary.Name, runErr, out)
	}
	var ok1, ok2, ok3, ok4 bool
	ca, ok1 = extractBlock(out, "<ca>", "</ca>")
	cert, ok2 = extractBlock(out, "<cert>", "</cert>")
	key, ok3 = extractBlock(out, "<key>", "</key>")
	ta, ok4 = extractBlock(out, "<tls-auth>", "</tls-auth>")
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return "", "", "", "", fmt.Errorf("could not parse cert material out of remote output:\n%s", out)
	}
	return ca, cert, key, ta, nil
}

func ovpnClientHeader() string {
	return "client\ndev tun\nresolv-retry infinite\nnobind\npersist-key\npersist-tun\n" +
		"remote-cert-tls server\ncipher AES-256-GCM\nkey-direction 1\nverb 3\n\n"
}

func ovpnCertBlocks(ca, cert, key, ta string) string {
	return fmt.Sprintf("<ca>\n%s\n</ca>\n<cert>\n%s\n</cert>\n<key>\n%s\n</key>\n<tls-auth>\n%s\n</tls-auth>\n\n", ca, cert, key, ta)
}

func nodeRemoteLine(n Node) string {
	proto := n.OVPNProto
	if proto == "" {
		proto = "udp"
	}
	port := n.OVPNPort
	if port == 0 {
		port = 1194
	}
	return fmt.Sprintf("%s %d %s", n.Host, port, proto)
}

// createOpenVPNCustomer builds ONE .ovpn with a <connection> block per node
// - automatic failover in list order, no user choice involved. Good when
// you just want reliability, not a pick-a-country experience.
func createOpenVPNCustomer(nodes []Node, customerName string) (string, error) {
	if len(nodes) == 0 {
		return "", fmt.Errorf("no nodes selected")
	}
	ca, cert, key, ta, err := issueSharedClientCert(nodes[0], customerName)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	b.WriteString(ovpnClientHeader())
	b.WriteString(ovpnCertBlocks(ca, cert, key, ta))
	for _, n := range nodes {
		fmt.Fprintf(&b, "<connection>\nremote %s\n</connection>\n", nodeRemoteLine(n))
	}
	return b.String(), nil
}

// createOpenVPNCustomerPerLocation issues ONE shared client cert (same CA,
// so it's valid everywhere) but returns a SEPARATE standalone .ovpn per
// node - the customer picks which file/profile to use in their client,
// rather than getting automatic failover in a fixed order.
func createOpenVPNCustomerPerLocation(nodes []Node, customerName string) (map[string]string, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes selected")
	}
	ca, cert, key, ta, err := issueSharedClientCert(nodes[0], customerName)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(nodes))
	for _, n := range nodes {
		var b bytes.Buffer
		b.WriteString(ovpnClientHeader())
		fmt.Fprintf(&b, "remote %s\n\n", nodeRemoteLine(n))
		b.WriteString(ovpnCertBlocks(ca, cert, key, ta))
		out[n.Name] = b.String()
	}
	return out, nil
}

func extractBlock(s, open, close string) (string, bool) {
	start := indexOf(s, open)
	if start < 0 {
		return "", false
	}
	start += len(open)
	end := indexOf(s[start:], close)
	if end < 0 {
		return "", false
	}
	return trimSpace(s[start : start+end]), true
}

func indexOf(s, substr string) int {
	n, m := len(s), len(substr)
	if m == 0 || m > n {
		return -1
	}
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == substr {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\n' || s[i] == '\r' || s[i] == '\t') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\n' || s[j-1] == '\r' || s[j-1] == '\t') {
		j--
	}
	return s[i:j]
}

// sshpassRun runs one command over password-authenticated SSH via sshpass,
// used only for the one-time key installation step during bootstrap.
func sshpassRun(password string, port int, target, remoteCmd string) (string, error) {
	args := []string{
		"-p", password,
		"ssh", "-o", "StrictHostKeyChecking=accept-new", "-o", "PreferredAuthentications=password",
		"-p", fmt.Sprintf("%d", port), target, remoteCmd,
	}
	cmd := exec.Command("sshpass", args...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	combined := out.String() + stderr.String()
	return combined, err
}

var pageTpl = template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Simorgh Node Panel</title>
<style>
body{font-family:system-ui,sans-serif;max-width:720px;margin:2rem auto;padding:0 1rem;background:#0f1115;color:#e6e6e6}
h1{font-size:1.3rem} h2{font-size:1.05rem;margin-top:2rem;border-bottom:1px solid #333;padding-bottom:.3rem}
label{display:block;margin:.6rem 0 .2rem;font-size:.9rem;color:#aaa}
input,select{width:100%;padding:.5rem;background:#1a1d24;border:1px solid #333;color:#eee;border-radius:4px;box-sizing:border-box}
button{margin-top:1rem;padding:.6rem 1.2rem;background:#3b6ef0;color:#fff;border:none;border-radius:4px;cursor:pointer}
table{width:100%;border-collapse:collapse;margin-top:.5rem}
td,th{text-align:left;padding:.4rem;border-bottom:1px solid #222;font-size:.9rem}
pre{background:#1a1d24;padding:1rem;border-radius:4px;overflow-x:auto;white-space:pre-wrap;font-size:.85rem}
.err{color:#ff6b6b} .ok{color:#6bd97a}
</style></head><body>
<h1>Simorgh Node Panel</h1>
<p style="color:#888;font-size:.85rem">Multi-location WireGuard configs without manual SSH. Each node below is a
server that already had <code>install.sh</code> run on it.</p>

{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if .Result}}
<h2>Result</h2>
<pre>{{.Result}}</pre>
{{end}}

<h2>Nodes</h2>
<table><tr><th>Name</th><th>Protocol</th><th>Host</th><th>SSH</th><th></th></tr>
{{range .Nodes}}
<tr><td>{{.Name}}</td><td>{{.Protocol}}</td><td>{{.Host}}</td><td>{{.SSHUser}}@{{.Host}}:{{.SSHPort}}</td>
<td><form method="POST" action="/nodes/remove" style="margin:0"><input type="hidden" name="name" value="{{.Name}}"><button style="padding:.2rem .6rem;background:#a33">remove</button></form></td></tr>
{{end}}
</table>

<h2>Add a node</h2>
<p style="color:#888;font-size:.85rem">Two ways: bootstrap a brand-new server with just its SSH login (installs
WireGuard and everything needed, then registers it) - or register a server you already set up yourself.</p>

<h3 style="font-size:.95rem;color:#ccc">Bootstrap a new node (SSH username + password)</h3>
<form method="POST" action="/nodes/bootstrap">
<label>Name (e.g. germany)</label><input name="name" required>
<label>Public host/IP</label><input name="host" required>
<label>SSH user</label><input name="ssh_user" value="root" required>
<label>SSH port</label><input name="ssh_port" value="22" required>
<label>SSH password (used once, to install our key - never stored)</label><input name="ssh_password" type="password" required>
<label>Protocol</label>
<select name="protocol" required>
<option value="wireguard">WireGuard</option>
<option value="openvpn">OpenVPN</option>
</select>
<label>WireGuard listen port (used if protocol=WireGuard)</label><input name="wg_port" value="51820">
<label>WireGuard subnet (used if protocol=WireGuard)</label><input name="wg_subnet" value="10.66.66.1/24">
<label>OpenVPN port (used if protocol=OpenVPN)</label><input name="ovpn_port" value="1194">
<label>OpenVPN proto (used if protocol=OpenVPN)</label>
<select name="ovpn_proto"><option value="udp">udp</option><option value="tcp">tcp</option></select>
<p style="color:#888;font-size:.8rem">OpenVPN: if you already have an OpenVPN node registered, this new one
automatically imports its CA - so one client cert works across all your OpenVPN locations. The first
OpenVPN node you bootstrap generates a fresh CA.</p>
<button type="submit">Bootstrap and add node</button>
</form>

<h3 style="font-size:.95rem;color:#ccc">Register an already-set-up node (SSH key already trusted)</h3>
<form method="POST" action="/nodes/add">
<label>Name (e.g. germany)</label><input name="name" required>
<label>Public host/IP (used both for SSH and the WireGuard Endpoint)</label><input name="host" required>
<label>SSH user</label><input name="ssh_user" value="root" required>
<label>SSH port</label><input name="ssh_port" value="22" required>
<button type="submit">Add node</button>
</form>

<h2>Create a WireGuard customer (single location)</h2>
<form method="POST" action="/customers/create">
<label>Node / location</label>
<select name="node" required>
{{range .Nodes}}{{if eq .Protocol "wireguard"}}<option value="{{.Name}}">{{.Name}} ({{.Host}})</option>{{end}}{{end}}
</select>
<label>Customer name</label><input name="customer_name" required>
<label>Bandwidth cap in Mbps (empty = unlimited - not yet wired for direct mode, see docs)</label><input name="bw_mbps">
<button type="submit">Create WireGuard config</button>
</form>

<h2>Create an OpenVPN customer (pick one or more locations)</h2>
<p style="color:#888;font-size:.85rem">One .ovpn file with a <code>&lt;connection&gt;</code> block per location
picked - OpenVPN itself fails over to the next one if a location is down.</p>
<form method="POST" action="/customers/create-openvpn">
{{range .Nodes}}{{if eq .Protocol "openvpn"}}
<label style="display:flex;align-items:center;gap:.4rem;font-weight:normal">
<input type="checkbox" name="nodes" value="{{.Name}}" style="width:auto"> {{.Name}} ({{.Host}}:{{.OVPNPort}}/{{.OVPNProto}})
</label>
{{end}}{{end}}
<label>Customer name</label><input name="customer_name" required>
<label>Mode</label>
<select name="mode">
<option value="perlocation">Separate file per location - customer picks which to use</option>
<option value="failover">One file, automatic failover in the order checked above</option>
</select>
<button type="submit">Create OpenVPN config(s)</button>
</form>

</body></html>`))

type pageData struct {
	Nodes  []Node
	Error  string
	Result string
}

func main() {
	dataPath := os.Getenv("NODEPANEL_DATA")
	if dataPath == "" {
		dataPath = "/etc/simorgh/nodepanel.json"
	}
	listenAddr := os.Getenv("NODEPANEL_LISTEN")
	if listenAddr == "" {
		listenAddr = "127.0.0.1:8787"
	}
	if err := os.MkdirAll("/etc/simorgh", 0700); err != nil {
		log.Printf("[warn] mkdir /etc/simorgh: %v", err)
	}

	s := newStore(dataPath)

	render := func(w http.ResponseWriter, d pageData) {
		d.Nodes = s.Nodes
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := pageTpl.Execute(w, d); err != nil {
			log.Printf("[error] template: %v", err)
		}
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		render(w, pageData{})
	})

	http.HandleFunc("/nodes/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		_ = r.ParseForm()
		var port int
		fmt.Sscanf(r.FormValue("ssh_port"), "%d", &port)
		if port == 0 {
			port = 22
		}
		s.addNode(Node{
			Name:     r.FormValue("name"),
			Host:     r.FormValue("host"),
			SSHUser:  r.FormValue("ssh_user"),
			SSHPort:  port,
			Protocol: "wireguard",
		})
		if err := s.save(); err != nil {
			render(w, pageData{Error: "save failed: " + err.Error()})
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	http.HandleFunc("/nodes/remove", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		_ = r.ParseForm()
		s.removeNode(r.FormValue("name"))
		_ = s.save()
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	http.HandleFunc("/nodes/bootstrap", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		_ = r.ParseForm()
		name := r.FormValue("name")
		host := r.FormValue("host")
		user := r.FormValue("ssh_user")
		password := r.FormValue("ssh_password")
		protocol := r.FormValue("protocol")
		var port int
		fmt.Sscanf(r.FormValue("ssh_port"), "%d", &port)
		if port == 0 {
			port = 22
		}

		var out string
		var err error
		node := Node{Name: name, Host: host, SSHUser: user, SSHPort: port, Protocol: protocol}

		switch protocol {
		case "openvpn":
			ovpnPort := r.FormValue("ovpn_port")
			ovpnProto := r.FormValue("ovpn_proto")
			out, err = bootstrapOpenVPNNode(s, host, port, user, password, ovpnPort, ovpnProto)
			var portNum int
			fmt.Sscanf(ovpnPort, "%d", &portNum)
			node.OVPNPort = portNum
			node.OVPNProto = ovpnProto
		default:
			node.Protocol = "wireguard"
			wgPort := r.FormValue("wg_port")
			wgSubnet := r.FormValue("wg_subnet")
			out, err = bootstrapWireGuardNode(host, port, user, password, wgPort, wgSubnet)
		}

		if err != nil {
			render(w, pageData{Error: err.Error()})
			return
		}

		s.addNode(node)
		if err := s.save(); err != nil {
			render(w, pageData{Result: out, Error: "node bootstrapped but save failed: " + err.Error()})
			return
		}
		render(w, pageData{Result: "Node bootstrapped and registered.\n\n" + out})
	})

	http.HandleFunc("/customers/create-openvpn", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		_ = r.ParseForm()
		custName := r.FormValue("customer_name")
		selected := r.Form["nodes"]
		if len(selected) == 0 {
			render(w, pageData{Error: "pick at least one OpenVPN node"})
			return
		}
		var nodes []Node
		for _, name := range selected {
			n, ok := s.find(name)
			if !ok {
				render(w, pageData{Error: "unknown node: " + name})
				return
			}
			nodes = append(nodes, n)
		}
		mode := r.FormValue("mode")
		if mode == "" {
			mode = "perlocation"
		}

		if mode == "failover" {
			out, err := createOpenVPNCustomer(nodes, custName)
			if err != nil {
				render(w, pageData{Error: err.Error()})
				return
			}
			render(w, pageData{Result: out})
			return
		}

		files, err := createOpenVPNCustomerPerLocation(nodes, custName)
		if err != nil {
			render(w, pageData{Error: err.Error()})
			return
		}
		var b bytes.Buffer
		for _, n := range nodes {
			fmt.Fprintf(&b, "═══ %s (%s-%s.ovpn) ═══\n%s\n\n", n.Name, custName, n.Name, files[n.Name])
		}
		render(w, pageData{Result: b.String()})
	})

	http.HandleFunc("/customers/create", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		_ = r.ParseForm()
		nodeName := r.FormValue("node")
		custName := r.FormValue("customer_name")
		bw := r.FormValue("bw_mbps")

		n, ok := s.find(nodeName)
		if !ok {
			render(w, pageData{Error: "unknown node: " + nodeName})
			return
		}
		out, err := runOnNode(n, custName, bw)
		if err != nil {
			render(w, pageData{Error: err.Error()})
			return
		}
		render(w, pageData{Result: out})
	})

	log.Printf("[nodepanel] listening on %s (data: %s)", listenAddr, dataPath)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}
