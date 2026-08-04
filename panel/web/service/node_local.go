package service

import (
	"context"
	"errors"
	"runtime"
	"sync"

	"github.com/mhsanaei/3x-ui/v2/config"
	"github.com/mhsanaei/3x-ui/v2/node"
)

// LocalRunner drives the machine the panel is running on.
//
// Every method here is an ADAPTER. The work is done by services that predate
// nodes entirely -- CoreService, XrayService, the per-protocol services -- and
// nothing in this file reimplements any of it. Routing the panel's own host
// through this type, rather than special-casing "local" above the Runner
// interface, is what proves the abstraction is behaviour-preserving: the
// existing suite already exercises these same paths, so if it still passes, the
// abstraction changed nothing.
type LocalRunner struct {
	coreService CoreService

	// mu guards lastHash. Apply can be called concurrently by the tick loop and
	// by an operator action in the same process.
	mu sync.Mutex
	// lastHash is the state this process last applied, and is deliberately
	// per-process rather than persisted: after a restart the panel cannot know
	// what state the daemons are actually in, so re-applying once is the correct
	// conservative answer.
	lastHash string
}

func NewLocalRunner() *LocalRunner { return &LocalRunner{} }

// errNotYetImplemented marks the methods filled in by later tasks. It is a
// distinct sentinel rather than a nil return so that wiring one of them up
// early fails loudly instead of silently reporting an empty result -- an empty
// CollectResult would read as "this node moved no traffic", which bills nobody.
var errNotYetImplemented = errors.New("node: not implemented yet")

// Status reports what this host is running.
//
// It must not error on a bare host: this is the first call a freshly
// bootstrapped node serves, and CoreService's probes are all written to answer
// "not installed" rather than fail when something is missing.
func (r *LocalRunner) Status(ctx context.Context) (node.NodeStatus, error) {
	cores := r.coreService.GetCoresStatus()
	out := make([]node.CoreStatus, 0, len(cores))
	for _, c := range cores {
		out = append(out, node.CoreStatus{
			Name:     c.Name,
			State:    string(c.State),
			Detail:   c.Detail,
			Version:  c.Version,
			Inbounds: c.Inbounds,
			Extra:    c.Extra,
		})
	}

	sys := r.coreService.GetSystemStatus()
	mods := make([]node.ModuleStatus, 0, len(sys.Modules))
	for _, m := range sys.Modules {
		mods = append(mods, node.ModuleStatus{Name: m.Name, Loaded: m.Loaded, Optional: m.Optional})
	}

	r.mu.Lock()
	hash := r.lastHash
	r.mu.Unlock()

	return node.NodeStatus{
		Version:   config.GetVersion(),
		Arch:      runtime.GOARCH,
		Distro:    distroPretty(),
		StateHash: hash,
		Cores:     out,
		System: node.SystemStatus{
			IpForward: sys.IpForward,
			Nftables:  sys.Nftables,
			Iproute:   sys.Iproute,
			Modules:   mods,
			ModulesOK: sys.ModulesOK,
		},
	}, nil
}

// Provision installs prerequisites for the named cores, streaming progress.
//
// CoreService.Provision is synchronous and returns every step at the end, so
// this runs it on its own goroutine and feeds the channel. That is a real
// improvement rather than plumbing: provisioning can install a kernel and
// repoint the bootloader, which takes minutes, and an operator watching a blank
// screen cannot tell that from a hang.
func (r *LocalRunner) Provision(ctx context.Context, cores []string) (<-chan node.ProvisionStep, error) {
	ch := make(chan node.ProvisionStep, 32)
	go func() {
		defer close(ch)
		for _, s := range r.coreService.Provision(cores) {
			select {
			case ch <- node.ProvisionStep{Name: s.Name, OK: s.OK, Warn: s.Warn, Msg: s.Msg, Log: s.Log}:
			case <-ctx.Done():
				// The caller has gone. Returning here rather than continuing to
				// push is what stops this goroutine outliving the request that
				// started it.
				return
			}
		}
	}()
	return ch, nil
}

// Logs returns recent output for one core. An unknown or never-started core
// yields "" rather than an error: the UI asks for every core's logs to render
// its list, and on any given host most of them are not running.
func (r *LocalRunner) Logs(ctx context.Context, core string) (string, error) {
	return r.coreService.CoreLogs(core), nil
}

// Apply is filled in by the next task, which needs BuildDesiredState to exist
// before it has anything to apply.
func (r *LocalRunner) Apply(ctx context.Context, state node.DesiredState) (node.ApplyResult, error) {
	return node.ApplyResult{}, errNotYetImplemented
}

// Collect and Enforce are filled in by the traffic-split task, which moves the
// local halves of XrayTrafficJob.Run behind them.
func (r *LocalRunner) Collect(ctx context.Context) (node.CollectResult, error) {
	return node.CollectResult{}, errNotYetImplemented
}

// Enforce with an empty set is the common case and must stay a no-op, so the
// emptiness check comes before the unimplemented return rather than after it.
func (r *LocalRunner) Enforce(ctx context.Context, disabled node.DisabledSet) error {
	if len(disabled.Emails) == 0 {
		return nil
	}
	return errNotYetImplemented
}
