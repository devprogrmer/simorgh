# Simorgh Panel — fork notice

This directory is a fork of [vpn-ui](https://github.com/Sir-MmD/vpn-ui) by
Sir-MmD, itself built on [3X-UI](https://github.com/mhsanaei/3x-ui) by
mhsanaei. All original credit for the panel's architecture, protocol
support, and the substantial engineering behind it belongs to those
projects and their contributors.

## License

This fork is **GPLv3**, same as upstream — see `LICENSE` in this directory.
This is a different, separate license from the MIT license covering
`core/` (the Simorgh tunnel) and `install.sh` elsewhere in this repository.
Do not assume MIT terms apply here; they don't. GPLv3's copyleft and
source-availability requirements carry over to this fork unchanged.

## What actually changed in this fork

- The panel's display name (`config/name`) was changed to "Simorgh". This
  is the single, central identifier the panel's `GetName()` reads from.
- Nothing else. No functional code, protocol handling, database schema,
  or UI logic was modified.

## What was deliberately NOT added, and why

A "multi-node" feature (one central panel managing protocol daemons on
other physical servers) was requested during this fork and **intentionally
not built**. This was verified against the actual code, not assumed:

`inbound.Listen` — the field that determines what address each protocol
daemon binds to — is used directly in local `net.Listen(...)` calls across
`web/service/wgc.go`, `awg.go`, `gre.go`, `openvpn.go`, `ssh.go`, and others.
A daemon can only bind to an address that belongs to a network interface on
the machine it's actually running on. There is no existing remote-agent or
API layer in this codebase for a panel on one machine to control a daemon
on another (the pattern projects like Marzban-node or Remnawave are built
around). Adding one is a real, separately-scoped project, not a small
patch — doing it hastily inside a fork of an actively-used panel risked
breaking a working system for a half-finished feature.

**What Simorgh's own tunnel already gives you toward the same goal**,
without touching this panel's architecture at all:

- `CUSTOMERS_FILE` multi-client mode (tested) lets many Iran-side customers
  share one foreign server.
- A client with multiple `REMOTE_IP` servers (tested) auto-picks whichever
  measures best, which covers "give customers the fastest location"
  automatically, without a manual per-location panel UI.
- For genuinely separate physical locations, deploy this same panel+core
  stack independently per location — the standard pattern real
  multi-location services use at the infrastructure level.

See `../docs/DEPLOYMENT.md` in the main Simorgh project for the fuller
write-up of this reasoning and the practical alternatives, including
`../nodepanel/` — a separate, small, tested tool built afterward that
handles multi-location WireGuard and OpenVPN (with real automatic
failover for OpenVPN) without touching this panel's code at all.

## Reseller support — already here, verified by reading the code

Unrelated to multi-node: this fork already has a real reseller system,
found by reading `database/model/model.go` directly (not assumed, not
from the README): a distinct `IsReseller` role, a per-reseller traffic
balance ledger (`ResellerProfile` — `AllowanceBytes`/`SpentBytes` under
transaction), policy levers (`DaysPerGB`, `MinCreateGB`/`MinAddGB`,
`Unlimited`), per-reseller inbound access scoping, and a dedicated
`manageResellers` permission. See `../docs/DEPLOYMENT.md` for the full
breakdown. This existed before this fork touched anything — it's upstream
functionality, surfaced here because it directly answers "can I resell
accounts," not something added by this fork.

## Testing status — read before deploying

**This fork has not been built or run in the environment it was created
in.** The build environment lacks network access to `golang.zx2c4.com` (one
of the panel's dependencies) and the Go toolchain available was older than
this project's required `go 1.26.2`. Only a plain-text edit was made (the
`config/name` file) — nothing that should plausibly break compilation, but
this was **not verified by actually building it**. Build and smoke-test
this yourself (`go build`, then run through the normal install flow) before
relying on it, the same way you would for any fork you hadn't personally
compiled yet.
