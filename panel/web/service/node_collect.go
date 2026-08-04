package service

import (
	"context"

	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/node"
	"github.com/mhsanaei/3x-ui/v2/web/service/rbridge"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

// The collection and enforcement halves of a traffic tick, as one machine
// performs them.
//
// XrayTrafficJob.Run used to do all of this inline, and reading it closely shows
// it was always two separable things:
//
//   - COLLECTION and ENFORCEMENT touch local kernel state -- Xray's gRPC stats,
//     nftables counters, RADIUS session maps, in-process relay tallies, peer
//     reconciles, session kills. These only work on the machine the traffic is
//     on, so they belong behind Runner.
//   - ACCOUNTING touches the database -- AddTraffic, quota crossing, the
//     reseller ledger. This must stay central, because the database is the only
//     place that knows an account's total across every node. Enforcing per node
//     would let a 10GB account spend 10GB on each of three.
//
// Splitting along that line is what makes one quota work across a fleet.

// SetRadius wires the shared RADIUS server into the per-protocol services this
// runner drives.
//
// Without it their radiusService is nil and every DisableClients /
// KillDisabledSessions call silently no-ops behind a `!= nil` guard -- which is
// why over-quota l2tp/pptp/openvpn tunnels were once never torn down live and
// only got refused at the next reconnect. The secret is irrelevant to the kill
// paths (they read the in-memory session map), so callers may pass empty.
func (r *LocalRunner) SetRadius(rs *RadiusService, secret string) {
	r.radiusService = rs
	r.l2tpService.SetRadius(rs, secret)
	r.pptpService.SetRadius(rs, secret)
	r.openvpnService.SetRadius(rs, secret)
	r.ocservService.SetRadius(rs, secret)
	r.sstpService.SetRadius(rs, secret)
	r.ikev2Service.SetRadius(rs, secret)

	// The rbridge Sweeper drives the protocols that authenticate locally with no
	// RADIUS round-trip: it polls their live tunnels and writes their sessions and
	// nft accounting through the RADIUS service.
	r.sweeper = rbridge.New(rs)
	r.sweeper.Register(&r.ikev2Service)
	r.sweeper.Register(&r.wgcService)
	r.sweeper.Register(&r.awgService)
	r.sweeper.Register(&r.greService)
}

// Collect reads and RESETS this machine's traffic counters.
//
// Reset-on-read makes the caller responsible for what it receives: a second
// Collect will not return the same bytes again, so a dropped result is traffic
// nobody is billed for. The master therefore persists before doing anything else
// with a tick.
func (r *LocalRunner) Collect(ctx context.Context) (node.CollectResult, error) {
	var res node.CollectResult

	if r.xrayService.IsXrayRunning() {
		traffics, clientTraffics, err := r.xrayService.GetXrayTraffic()
		if err == nil {
			res.Traffics = traffics
			res.ClientTraffics = clientTraffics
		}
	}

	// Reconcile the kernel peer sets to database state BEFORE the sweep polls, so
	// each interface holds exactly the enabled, non-disabled devices. Because a
	// removed peer cannot complete a handshake, this makes disable/quota
	// enforcement HARD for the WireGuard family rather than eventual.
	if err := r.wgcService.GenerateAllConfigs(); err != nil {
		logger.Debug("wgc: peer reconcile failed:", err)
	}
	if err := r.awgService.GenerateAllConfigs(); err != nil {
		logger.Debug("awg: peer reconcile failed:", err)
	}
	// GRE matters MORE here than the WireGuard family: it has no handshake to
	// refuse, so this reconcile IS the enforcement -- it deletes a disabled
	// account's point-to-point device and withdraws its route, leaving no reverse
	// path at all.
	if err := r.greService.GenerateAllConfigs(); err != nil {
		logger.Debug("gre: peer reconcile failed:", err)
	}
	r.ikev2Service.ReconcileDisabled()

	if r.sweeper != nil {
		r.sweeper.Tick()
	}

	if r.radiusService != nil {
		vpnSessions := map[string]map[string]string{
			"l2tp":        r.radiusService.GetSessions("l2tp"),
			"pptp":        r.radiusService.GetSessions("pptp"),
			"openvpn":     r.radiusService.GetSessions("openvpn"),
			"openconnect": r.radiusService.GetSessions("openconnect"),
			"sstp":        r.radiusService.GetSessions("sstp"),
			"ikev2":       r.radiusService.GetSessions("ikev2"),
			"wg-c":        r.radiusService.GetSessions("wg-c"),
			"awg":         r.radiusService.GetSessions("awg"),
			"gre":         r.radiusService.GetSessions("gre"),
		}
		res.ClientTraffics = append(res.ClientTraffics,
			r.nftService.CollectAndResetTraffic(vpnSessions)...)
	}

	// MTProto and SSH are userspace relays: no client gets a tunnel IP, so
	// neither can use the nft per-IP path. Each keeps its own per-account byte
	// counters, but both ALSO egress through a paired Xray socks inbound whose
	// username is the account email, so Xray has usually already billed those
	// exact bytes in this same tick.
	//
	// The predicate is "does this account already have a record", NOT "did it
	// move bytes". The two sources flush on different boundaries: Xray can report
	// zero for an account whose bytes the relay already tallied, then report them
	// a tick or two later. Gating on a positive count admits the relay copy now
	// and the Xray copy afterwards -- measured at 1.67x on a 100MiB SSH pull.
	res.ClientTraffics = appendUnrecordedTraffic(res.ClientTraffics,
		append(r.mtprotoService.CollectTraffic(), r.sshService.CollectTraffic()...))

	return res, nil
}

// appendUnrecordedTraffic appends each fallback record whose email has no record
// in `existing` yet. See Collect for why presence, not byte count, is the test.
func appendUnrecordedTraffic(existing, fallback []*xray.ClientTraffic) []*xray.ClientTraffic {
	recorded := make(map[string]bool, len(existing))
	for _, t := range existing {
		recorded[t.Email] = true
	}
	for _, t := range fallback {
		if recorded[t.Email] {
			continue
		}
		// Marked as we go so two relays reporting the same account in one tick
		// contribute once, not twice.
		recorded[t.Email] = true
		existing = append(existing, t)
	}
	return existing
}

// Enforce stops serving the named accounts.
//
// Level-triggered: the caller sends the WHOLE disabled set every tick. The
// edge-triggered kills that used to sit inline only fired on the exact tick a
// quota was first crossed, so a missed one left the live tunnel running until
// the user reconnected -- the "used their 1GB but kept going" report. Re-deriving
// the set centrally each tick makes it idempotent and cheap when nothing is
// disabled.
func (r *LocalRunner) Enforce(ctx context.Context, disabled node.DisabledSet) error {
	// The sweep is level-triggered and runs even for an empty set: an account can
	// be disabled in settings rather than by quota, and that must still tear down
	// its live session.
	r.l2tpService.KillDisabledSessions()
	r.pptpService.KillDisabledSessions()
	r.openvpnService.KillDisabledSessions()
	r.ocservService.KillDisabledSessions()
	r.sstpService.KillDisabledSessions()
	r.ikev2Service.KillDisabledSessions()
	// MTProto re-renders [access.user_enabled] from client_traffics; telemt's
	// config watcher cancels the disabled accounts' live sessions itself, so
	// there is no separate kill path and no daemon restart.
	r.mtprotoService.KillDisabledSessions()
	// SSH: the server owns every net.Conn, so this is an in-process close.
	r.sshService.KillDisabledSessions()

	if len(disabled.Emails) == 0 {
		return nil
	}
	// The protocols whose credentials live in a file the daemon reads need that
	// file rewritten as well as the session killed, or the account reconnects.
	r.l2tpService.DisableClients(disabled.Emails)
	r.pptpService.DisableClients(disabled.Emails)
	r.openvpnService.DisableClients(disabled.Emails)
	return nil
}
