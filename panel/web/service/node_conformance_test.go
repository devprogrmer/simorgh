package service

import (
	"context"
	"crypto/tls"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/node"
)

// The conformance suite: one set of assertions, run against BOTH runners.
//
// LocalRunner calls the services in-process; the client talks to a node.Server
// wrapping a LocalRunner over a real mTLS listener. Every feature above the
// Runner interface is written once, so the two implementations agreeing is not a
// nice-to-have -- a protocol that works locally and silently not on a node is
// worse than one that works nowhere, because the panel keeps reporting success.
//
// Discipline does not keep two implementations in step. This suite does.

func conformanceRunners(t *testing.T) map[string]node.Runner {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	caCert, caKey, err := node.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	srvCert, srvKey, err := node.IssueNodeCert(caCert, caKey, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	cliCert, cliKey, err := node.IssueMasterCert(caCert, caKey)
	if err != nil {
		t.Fatal(err)
	}

	tlsCfg, err := node.NodeTLSConfig(caCert, srvCert, srvKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(node.NewServer(NewLocalRunner()).Handler())
	srv.TLS = tlsCfg
	srv.StartTLS()
	t.Cleanup(srv.Close)

	addr := srv.Listener.Addr().String()
	host, port, err := splitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	client, err := node.NewClient(node.ClientConfig{
		Address: host, Port: port,
		CACert: caCert, Cert: cliCert, Key: cliKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	return map[string]node.Runner{"local": NewLocalRunner(), "remote": client}
}

func splitHostPort(addr string) (string, int, error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	n, err := strconv.Atoi(p)
	return h, n, err
}

// Status must answer identically in shape from both runners: same version, same
// architecture, and the same set of cores. A node whose Status disagrees with
// the master's own would make the cores list mean two different things
// depending on where it was read.
func TestRunnerConformanceStatus(t *testing.T) {
	runners := conformanceRunners(t)
	var first *node.NodeStatus
	for name, r := range runners {
		st, err := r.Status(context.Background())
		if err != nil {
			t.Fatalf("%s: Status: %v", name, err)
		}
		if st.Version == "" || st.Arch == "" {
			t.Errorf("%s: Status must always carry version and arch, got %+v", name, st)
		}
		if first == nil {
			cp := st
			first = &cp
			continue
		}
		if st.Version != first.Version || st.Arch != first.Arch {
			t.Errorf("%s: version/arch differ from the other runner: %q/%q vs %q/%q",
				name, st.Version, st.Arch, first.Version, first.Arch)
		}
		if len(st.Cores) != len(first.Cores) {
			t.Errorf("%s: reports %d cores, the other reports %d", name, len(st.Cores), len(first.Cores))
		}
	}
}

// An empty enforce is the overwhelmingly common tick and must be a cheap no-op
// on both sides, or a healthy panel fills its logs with failures.
func TestRunnerConformanceEnforceEmpty(t *testing.T) {
	for name, r := range conformanceRunners(t) {
		if err := r.Enforce(context.Background(), node.DisabledSet{}); err != nil {
			t.Errorf("%s: empty enforce must be a no-op: %v", name, err)
		}
	}
}

// Logs for a core that is not running is an empty string, not an error: the UI
// asks for every core to render its list, and most are not running on any host.
func TestRunnerConformanceLogsUnknownCore(t *testing.T) {
	for name, r := range conformanceRunners(t) {
		out, err := r.Logs(context.Background(), "no-such-core")
		if err != nil {
			t.Errorf("%s: logs for an unknown core must not error: %v", name, err)
		}
		if out != "" {
			t.Errorf("%s: logs for an unknown core = %q; want empty", name, out)
		}
	}
}

// Provision must stream and then close its channel on both sides. A channel
// that never closes hangs the caller; one that closes without the transport
// having flushed loses the steps the operator is watching for.
func TestRunnerConformanceProvisionCloses(t *testing.T) {
	for name, r := range conformanceRunners(t) {
		ch, err := r.Provision(context.Background(), nil)
		if err != nil {
			t.Errorf("%s: Provision with no cores must not error: %v", name, err)
			continue
		}
		done := make(chan struct{})
		go func() {
			for range ch {
			}
			close(done)
		}()
		select {
		case <-done:
		case <-context.Background().Done():
			t.Errorf("%s: Provision channel never closed", name)
		}
	}
}

// Errors must PROPAGATE across the transport rather than being swallowed into a
// zero value. Apply is not implemented for a non-empty state yet, so both
// runners must say so -- the remote one included. A transport that turned a
// failure into an empty result would read as success and, for Collect, as "this
// node moved no traffic", billing nobody, silently, forever.
func TestRunnerConformanceErrorsPropagate(t *testing.T) {
	for name, r := range conformanceRunners(t) {
		_, err := r.Apply(context.Background(), node.DesiredState{
			Generation: 1,
			Inbounds:   []node.NodeInbound{{InboundId: 1, Tag: "x", Protocol: "wg-c", Port: 51820, Enable: true}},
		})
		if err == nil {
			t.Errorf("%s: Apply is unimplemented for a non-empty state and must report an error", name)
		}
	}
}

// Collect must behave identically on both sides. On a host with nothing
// configured it returns an empty result and no error, and the remote path must
// not turn that into a failure -- nor a failure into an empty result, which is
// the direction that silently bills nobody.
func TestRunnerConformanceCollectOnIdleHost(t *testing.T) {
	for name, r := range conformanceRunners(t) {
		res, err := r.Collect(context.Background())
		if err != nil {
			t.Errorf("%s: Collect on an idle host must not error: %v", name, err)
			continue
		}
		if len(res.Traffics) != 0 || len(res.ClientTraffics) != 0 {
			t.Errorf("%s: idle host reported traffic: %+v", name, res)
		}
	}
}

// A state whose generation does not advance is refused by the node. This is what
// stops a captured payload from being replayed to roll a node back to a revoked
// client set -- an attacker who can record one Apply could otherwise restore a
// deleted account by sending it again.
func TestNodeRefusesReplayedGeneration(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}
	caCert, caKey, _ := node.GenerateCA()
	srvCert, srvKey, _ := node.IssueNodeCert(caCert, caKey, "127.0.0.1")
	cliCert, cliKey, _ := node.IssueMasterCert(caCert, caKey)
	tlsCfg, _ := node.NodeTLSConfig(caCert, srvCert, srvKey)

	srv := httptest.NewUnstartedServer(node.NewServer(NewLocalRunner()).Handler())
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()

	host, port, _ := splitHostPort(srv.Listener.Addr().String())
	client, err := node.NewClient(node.ClientConfig{
		Address: host, Port: port, CACert: caCert, Cert: cliCert, Key: cliKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	// An empty state applies cleanly, which advances the generation to 5.
	if _, err := client.Apply(context.Background(), node.DesiredState{Generation: 5}); err != nil {
		t.Fatalf("first apply should succeed: %v", err)
	}
	// The same generation again, and an older one, must both be refused.
	for _, gen := range []int64{5, 4, 1} {
		_, err := client.Apply(context.Background(), node.DesiredState{Generation: gen})
		if err == nil {
			t.Errorf("generation %d was accepted after 5; a replayed state must be refused", gen)
		} else if !strings.Contains(strings.ToLower(err.Error()), "generation") {
			t.Errorf("generation %d refused with an unhelpful error: %v", gen, err)
		}
	}
	// Moving forward still works.
	if _, err := client.Apply(context.Background(), node.DesiredState{Generation: 6}); err != nil {
		t.Errorf("a newer generation must still be accepted: %v", err)
	}
}

// A client without a master-signed certificate must not reach the node API at
// all. Duplicated deliberately from the CA tests: this asserts the SERVER wires
// the TLS config in, not merely that the config would be correct if used.
func TestNodeServerRefusesUnauthenticatedClient(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}
	caCert, caKey, _ := node.GenerateCA()
	srvCert, srvKey, _ := node.IssueNodeCert(caCert, caKey, "127.0.0.1")
	tlsCfg, _ := node.NodeTLSConfig(caCert, srvCert, srvKey)

	srv := httptest.NewUnstartedServer(node.NewServer(NewLocalRunner()).Handler())
	srv.TLS = tlsCfg
	srv.StartTLS()
	defer srv.Close()

	host, port, _ := splitHostPort(srv.Listener.Addr().String())
	foreignCA, foreignKey, _ := node.GenerateCA()
	intruderCert, intruderKey, _ := node.IssueMasterCert(foreignCA, foreignKey)
	intruder, err := node.NewClient(node.ClientConfig{
		Address: host, Port: port, CACert: caCert, Cert: intruderCert, Key: intruderKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := intruder.Status(context.Background()); err == nil {
		t.Fatal("a client with a foreign certificate reached the node API")
	}
	_ = tls.VersionTLS13
}
