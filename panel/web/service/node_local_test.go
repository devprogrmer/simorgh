package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/node"
)

// Compile-time proof that LocalRunner satisfies the interface. Catching a gap
// here costs a build; catching it at runtime costs a production node that
// reports success while doing nothing.
var _ node.Runner = (*LocalRunner)(nil)

// newLocalRunner gives the runner the database its adapted services need.
//
// A node needs one too, and that is a real design point rather than test
// scaffolding: every protocol service reads its inbounds through
// database.GetDB() (wgc, openvpn, l2tp, gre, mtproto, ssh and ikev2 all do), so
// a node without a database could not run any of them without those drivers
// being rewritten -- which is the one thing this design exists to avoid. The
// node's database is a MATERIALISATION of the desired state it was pushed, not
// a source of truth: no reseller ledger, no admin users, no traffic history of
// record. Those stay on the master.
func newLocalRunner(t *testing.T) *LocalRunner {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	return NewLocalRunner()
}

// Status must work on a bare host with nothing configured. This is the very
// first call a freshly bootstrapped node answers, so erroring here would make
// every healthy new node look broken at the moment an operator is deciding
// whether the feature works.
func TestLocalRunnerStatusOnEmptyHost(t *testing.T) {
	st, err := newLocalRunner(t).Status(context.Background())
	if err != nil {
		t.Fatalf("Status on an unconfigured host must not error: %v", err)
	}
	if st.Version == "" {
		t.Error("Status must always report a version; the master refuses to push state to a node whose version it cannot read")
	}
	if st.Arch == "" {
		t.Error("Status must always report an architecture; bootstrap picks the binary from it")
	}
}

// Enforce with nobody to disable is the overwhelmingly common tick. It must be
// a cheap no-op rather than an error, or a perfectly healthy panel fills its
// logs with failures and real ones stop being visible.
func TestLocalRunnerEnforceEmptyIsNoOp(t *testing.T) {
	if err := newLocalRunner(t).Enforce(context.Background(), node.DisabledSet{}); err != nil {
		t.Fatalf("empty enforce should be a no-op: %v", err)
	}
}

// A cancelled context must stop Provision's producer rather than leaving a
// goroutine blocked forever on a channel nobody reads. Provisioning can install
// a kernel, so it is long-running by nature and cancellation is the normal way
// it ends early.
func TestLocalRunnerProvisionRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := newLocalRunner(t).Provision(ctx, nil)
	if err != nil {
		t.Fatalf("Provision with no cores must not error: %v", err)
	}
	cancel()
	// Draining must terminate: the producer either finishes or gives up on ctx.
	for range ch {
	}
}

// Logs for a core that was never started is an empty string, not an error: the
// UI asks for logs of every core to render the list, and most of them are not
// running on any given host.
func TestLocalRunnerLogsUnknownCoreIsEmpty(t *testing.T) {
	out, err := newLocalRunner(t).Logs(context.Background(), "a-core-that-does-not-exist")
	if err != nil {
		t.Fatalf("logs for an unknown core must not error: %v", err)
	}
	if out != "" {
		t.Errorf("logs for an unknown core = %q; want empty", out)
	}
}
