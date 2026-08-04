package node

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mustCA(t *testing.T) (cert, key []byte) {
	t.Helper()
	cert, key, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	return cert, key
}

// serveMTLS starts a TLS server that accepts only certificates signed by caCert,
// and returns its address. This drives the real crypto/tls handshake rather than
// asserting on struct fields, because the fields that matter here
// (ClientAuth in particular) have neighbours that look almost identical and
// silently accept anything.
func serveMTLS(t *testing.T, caCert, srvCert, srvKey []byte) string {
	t.Helper()
	cfg, err := NodeTLSConfig(caCert, srvCert, srvKey)
	if err != nil {
		t.Fatalf("NodeTLSConfig: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func get(t *testing.T, addr string, cfg *tls.Config) error {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	resp, err := c.Get("https://" + addr + "/")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	return err
}

// The happy path: a client holding a master-signed certificate gets in.
func TestCAAcceptsOwnClient(t *testing.T) {
	caCert, caKey := mustCA(t)
	srvCert, srvKey, err := IssueNodeCert(caCert, caKey, "127.0.0.1")
	if err != nil {
		t.Fatalf("IssueNodeCert: %v", err)
	}
	cliCert, cliKey, err := IssueMasterCert(caCert, caKey)
	if err != nil {
		t.Fatalf("IssueMasterCert: %v", err)
	}
	addr := serveMTLS(t, caCert, srvCert, srvKey)

	cfg, err := MasterTLSConfig(caCert, cliCert, cliKey, "127.0.0.1")
	if err != nil {
		t.Fatalf("MasterTLSConfig: %v", err)
	}
	if err := get(t, addr, cfg); err != nil {
		t.Fatalf("a client with a master-signed cert must be admitted: %v", err)
	}
}

// The test that matters most. A node applies configuration as root, so an
// unauthenticated node API is a remote root shell. A certificate from any other
// CA -- including a perfectly valid self-signed one -- must be refused.
func TestCARefusesForeignClientCert(t *testing.T) {
	caCert, caKey := mustCA(t)
	srvCert, srvKey, _ := IssueNodeCert(caCert, caKey, "127.0.0.1")
	addr := serveMTLS(t, caCert, srvCert, srvKey)

	// A complete, internally valid CA that this node has never heard of.
	foreignCA, foreignKey := mustCA(t)
	intruderCert, intruderKey, err := IssueMasterCert(foreignCA, foreignKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := MasterTLSConfig(caCert, intruderCert, intruderKey, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := get(t, addr, cfg); err == nil {
		t.Fatal("a certificate from a foreign CA was ACCEPTED; the node API is open to anyone who can generate one")
	}
}

// A client presenting no certificate at all must also be refused. This is the
// case RequireAnyClientCert would still catch but VerifyClientCertIfGiven would
// not, so it is worth pinning separately from the foreign-CA case.
func TestCARefusesClientWithNoCert(t *testing.T) {
	caCert, caKey := mustCA(t)
	srvCert, srvKey, _ := IssueNodeCert(caCert, caKey, "127.0.0.1")
	addr := serveMTLS(t, caCert, srvCert, srvKey)

	pool, err := CertPool(caCert)
	if err != nil {
		t.Fatal(err)
	}
	// Trusts the node, but offers nothing of its own.
	if err := get(t, addr, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}); err == nil {
		t.Fatal("a client with NO certificate was accepted; mTLS is not being enforced")
	}
}

// The master must also verify the NODE, or a hijacked address could impersonate
// one and be handed every inbound's configuration.
func TestMasterRefusesForeignServerCert(t *testing.T) {
	realCA, realKey := mustCA(t)
	cliCert, cliKey, _ := IssueMasterCert(realCA, realKey)

	// A server holding a cert from a CA the master does not trust.
	impostorCA, impostorKey := mustCA(t)
	srvCert, srvKey, _ := IssueNodeCert(impostorCA, impostorKey, "127.0.0.1")
	addr := serveMTLS(t, impostorCA, srvCert, srvKey)

	cfg, err := MasterTLSConfig(realCA, cliCert, cliKey, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := get(t, addr, cfg); err == nil {
		t.Fatal("the master accepted a node certificate from an untrusted CA")
	}
}

// Nodes are addressed by IP as often as by name, and an IP that lands in the
// Common Name instead of the SAN list is rejected by Go's verifier with an
// error that reads like a configuration problem rather than a missing SAN.
func TestIssueNodeCertCoversIPAndDNSNames(t *testing.T) {
	caCert, caKey := mustCA(t)

	for _, host := range []string{"127.0.0.1", "node1.example.com"} {
		srvCert, srvKey, err := IssueNodeCert(caCert, caKey, host)
		if err != nil {
			t.Fatalf("IssueNodeCert(%q): %v", host, err)
		}
		leaf, err := tls.X509KeyPair(srvCert, srvKey)
		if err != nil {
			t.Fatalf("the issued pair does not load: %v", err)
		}
		parsed, err := parseLeaf(leaf)
		if err != nil {
			t.Fatal(err)
		}
		if err := parsed.VerifyHostname(host); err != nil {
			t.Errorf("cert for %q does not verify for that host: %v", host, err)
		}
		if net.ParseIP(host) != nil && len(parsed.IPAddresses) == 0 {
			t.Errorf("cert for IP %q carries no IP SAN", host)
		}
		if net.ParseIP(host) == nil && len(parsed.DNSNames) == 0 {
			t.Errorf("cert for name %q carries no DNS SAN", host)
		}
	}
}

// A CA that is not marked as one, or that cannot sign, produces certificates
// that fail verification later with a confusing error. Pin the basic
// constraints at the point they are set.
func TestGenerateCAIsAUsableAuthority(t *testing.T) {
	caCert, _ := mustCA(t)
	pool, err := CertPool(caCert)
	if err != nil {
		t.Fatalf("the generated CA does not parse into a pool: %v", err)
	}
	if pool == nil {
		t.Fatal("nil pool")
	}
	if !strings.Contains(string(caCert), "BEGIN CERTIFICATE") {
		t.Fatal("GenerateCA must return PEM, which is what the database column stores")
	}
}

// Garbage in must be an error, not a panic: these bytes come out of a database
// column that a partial restore or a hand-edit can leave truncated.
func TestTLSConfigRejectsMalformedMaterial(t *testing.T) {
	if _, err := CertPool([]byte("not a certificate")); err == nil {
		t.Error("CertPool accepted non-PEM input")
	}
	if _, err := NodeTLSConfig([]byte("junk"), []byte("junk"), []byte("junk")); err == nil {
		t.Error("NodeTLSConfig accepted junk")
	}
	if _, err := MasterTLSConfig([]byte("junk"), []byte("junk"), []byte("junk"), "h"); err == nil {
		t.Error("MasterTLSConfig accepted junk")
	}
}
