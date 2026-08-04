package node

import "context"

// Runner is how the master drives one machine.
//
// Two implementations exist and must stay interchangeable: LocalRunner calls the
// panel's existing services in-process, RemoteRunner marshals the same calls
// over mTLS. Every feature above this line is therefore written once, which is
// the whole reason the panel's own host is modelled as a Node rather than as the
// absence of one.
//
// A single conformance suite runs the same assertions against both
// implementations; that suite, not discipline, is what stops them drifting
// apart. A protocol that works locally but silently not on a node is worse than
// one that works nowhere, because the panel keeps reporting success.
type Runner interface {
	// Apply makes the machine match state, idempotently.
	//
	// Returning without error means every inbound in ApplyResult.Applied is
	// running and every one in ApplyResult.Errors is not. A partial failure is a
	// normal RESULT, not an error return: one inbound whose port is taken must
	// not mark the other seventeen failed.
	Apply(ctx context.Context, state DesiredState) (ApplyResult, error)

	// Collect reads and RESETS this machine's traffic counters.
	//
	// Reset-on-read makes the caller responsible for what it receives: a second
	// Collect will not return the same bytes again, so a caller that drops a
	// result has lost that traffic permanently. This is why the master persists
	// before it does anything else with a tick.
	Collect(ctx context.Context) (CollectResult, error)

	// Enforce stops serving the named accounts.
	//
	// Level-triggered: the caller sends the WHOLE disabled set every tick, so a
	// missed message self-corrects on the next one rather than leaving an
	// over-quota account running until it happens to reconnect.
	Enforce(ctx context.Context, disabled DisabledSet) error

	// Provision installs prerequisites for the named cores, streaming progress.
	// The channel is closed when provisioning finishes.
	Provision(ctx context.Context, cores []string) (<-chan ProvisionStep, error)

	// Status reports what the machine is running. It must answer on a bare host
	// with nothing configured, since it is the first call a freshly bootstrapped
	// node serves.
	Status(ctx context.Context) (NodeStatus, error)

	// Logs returns recent output for one core.
	Logs(ctx context.Context, core string) (string, error)
}
