# Deployment: Prerequisites, Server Optimization, and Troubleshooting

## Scope of this document

This covers the **WireGuard** protocol installer (`protocols/wireguard.sh`) and
Simorgh's **multi-client server mode** (`CUSTOMERS_FILE`). OpenVPN and L2TP/IPsec
installers are not built yet — this document will be extended when they are.
Don't assume the troubleshooting entries below apply to those protocols.

"Works without errors" means every failure mode listed here has a known,
tested fix — not that no server-specific surprise can ever occur. Different
VPS providers, kernels, and OS images will occasionally do something this
list doesn't cover; if you hit that, the right move is to get the actual
error text and work from there, the same way we debugged the L2TP issue
earlier in this project.

## Prerequisites

### OS support

The WireGuard installer auto-detects and supports:
- Debian/Ubuntu (`apt`)
- RHEL-family (`yum`, with an EPEL fallback attempt on older releases)

Anything else fails loudly with a clear message rather than guessing at
package names.

### Packages

- `wireguard-tools` (installed automatically)
- `iproute2`, `iptables`, `python3` (installed automatically by `install.sh`
  if missing — `python3` is used for safe JSON editing of the customer list)
- Docker (installed automatically by `install.sh` if missing)

### Kernel

WireGuard needs either:
- A kernel with `CONFIG_WIREGUARD` built in (Linux 5.6+, which is the
  default on any current Ubuntu/Debian release), or
- The out-of-tree `wireguard-dkms` module on older kernels.

**This matters for containerized/virtualized VPS providers**: some
lightweight container-based hosting (as opposed to real or fully-virtualized
KVM/Xen VPS) does not expose a loadable WireGuard kernel module at all — no
install script can fix this, since it's a property of the host, not the
guest. If `wg-quick@wg0` won't come up and `journalctl` shows something
about the `wireguard` module, check with your provider whether they support
WireGuard at all before troubleshooting further.

### Firewall (cloud-provider level)

Every major provider (Hetzner, DigitalOcean, AWS Security Groups, Vultr,
etc.) has its own firewall **separate from the server's own iptables** —
this is the single most common cause of "it's configured correctly but
nothing connects." Open, inbound, on whichever server runs WireGuard:

- **UDP** on your chosen `ListenPort` (`51820` by default)
- If also running Simorgh's ICMP transport for the customer's carrier hop:
  ICMP must be allowed between the customer and this server too

### sysctls

- `net.ipv4.ip_forward = 1` — the installer sets this and persists it in
  `/etc/sysctl.conf`, but if you've overridden sysctl handling (e.g. with
  a config management tool that resets it on every run), make sure nothing
  reverts it after install.

## Integrating with an existing panel (3X-UI / vpn-ui family)

If you're already running a 3X-UI-based panel (including forks like
`vpn-ui`) rather than building a panel from scratch, Simorgh's multi-client
mode is designed to plug in underneath it as a carrier, filling two
specific, verified gaps in that panel family:

- **Per-customer bandwidth (Mbps) limiting**: 3X-UI supports total traffic
  quotas (GB) and IP-count limits, but not live speed throttling — this is
  a long-standing, still-open upstream feature request, not a
  misconfiguration on your end. Simorgh's `bandwidth_mbps` per customer in
  `CUSTOMERS_FILE` is a real, tested token-bucket limiter that covers this,
  as long as that customer's traffic actually flows through their Simorgh
  carrier client rather than connecting to the panel directly.
- **Device limiting by hardware ID**: not achievable with standard
  protocols (WireGuard, OpenVPN, L2TP, Xray/VMess/VLESS) or their standard
  clients — none of them transmit a device fingerprint as part of the
  protocol. The closest real levers are IP-count limiting (built into
  3X-UI) or a session-count limit; genuine HWID binding would require
  distributing your own custom client app, which is a materially different
  (and much larger) project than a panel integration.

**Correction**: an earlier version of this document claimed reseller/
sub-admin roles weren't present — that was wrong, based only on reading
the upstream README rather than the actual code. This specific fork
(`panel/`) has a real, fairly sophisticated reseller system, confirmed by
reading `database/model/model.go` directly:

- A distinct `IsReseller` role (not just a permission bit) — a reseller
  sees only the clients they created, scoped by role rather than a mask.
- A real traffic-balance ledger per reseller (`ResellerProfile`):
  `AllowanceBytes` minus `SpentBytes` tracked under transaction on every
  account create/edit/delete.
- Per-reseller policy levers: `DaysPerGB` (auto-derive expiry from volume
  sold), `MinCreateGB`/`MinAddGB` (floors on account size and top-ups),
  `Unlimited` (opt out of the balance check).
- Per-reseller inbound access scoping (`InboundAccess`) — an admin decides
  which inbounds/protocols each reseller can sell on.
- A dedicated `manageResellers` permission and (per the code) a Resellers
  management page.

This was verified by reading the actual Go model, not by running the
panel — the exact UI location/flow for managing this hasn't been
click-tested in this environment (same build limitation noted throughout
this document). Once you have the panel running, look for "Resellers" in
the admin menu.

**What's still accurate**: the *multi-node* limitation above (a reseller
in this panel sells accounts on inbounds on the single server it's
installed on — this reseller system doesn't extend across physical
servers, since that's the separate multi-node problem described earlier
in this document). Device limiting by hardware ID is also still accurate
as written above.

## Resilience: TRANSPORT=auto and running both transports on the server

`TRANSPORT=auto` (client-only) tries ICMP first, then UDP, and locks onto
whichever one actually reaches a server — useful when a link degrades in a
way that specifically breaks one transport (a path that starts dropping
ICMP, for instance) without the operator having to guess which one to
hardcode. Verified: a client set to `auto` correctly picked ICMP when only
ICMP worked, and correctly fell back to UDP when only UDP worked, in real
tests during development.

For this to actually have something to fall back *to*, the server side
needs to be reachable on both transports simultaneously. Since one Simorgh
process only binds one transport at a time, run two:

```bash
# ICMP instance
docker run -d --name simorgh_icmp --cap-add=NET_ADMIN --cap-add=NET_RAW \
  --device /dev/net/tun:/dev/net/tun --net=host --restart unless-stopped \
  -e TRANSPORT=icmp -e PASSWORD=... simorgh-core:latest

# UDP instance, different port, same password
docker run -d --name simorgh_udp --cap-add=NET_ADMIN --cap-add=NET_RAW \
  --device /dev/net/tun:/dev/net/tun --net=host --restart unless-stopped \
  -e TRANSPORT=udp -e UDP_PORT=51900 -e PASSWORD=... simorgh-core:latest
```

Both can point at the same `CUSTOMERS_FILE` in multi-client/forward mode
too — nothing about multi-client is transport-specific.

## Multi-location deployments (without panel-level node orchestration)

`vpn-ui` (and base 3X-UI) has **no built-in multi-node/multi-location
support** — this was checked directly against the panel's source: there is
no "node" concept anywhere in the codebase. It manages inbounds only on the
single host it's installed on. Building real central-controller/remote-node
orchestration into it would be a project on the scale of what Remnawave or
Marzban-node exist specifically to provide — not a small patch, and not
something to bolt onto a production panel without a much larger, separately
scoped effort.

**What you can do today, with zero new code**: deploy the same stack
(panel + protocol cores + Simorgh) independently on each foreign location
you want to offer, and hand customers whichever location's config they
choose. This is the same pattern real multi-location VPN services use at
the infrastructure level — "multi-location" is a deployment topology, not
a single piece of software.

Two things already built here reduce how much this costs to run in
practice:

- **Many Iran-side customers sharing one foreign server** (`CUSTOMERS_FILE`,
  tested with concurrent isolated customers) — you don't need a separate
  foreign VPS per customer, only per *location* you want to offer.
- **A client with 2+ `REMOTE_IP` servers auto-picks whichever measures
  best** — if what you actually want is "always fast," rather than "let the
  customer manually choose a country," pointing one client at several
  same-purpose foreign servers already gets you that automatically, without
  any panel UI for location selection at all.

## Troubleshooting (WireGuard)

| Symptom | Likely cause | Fix |
|---|---|---|
| `wg-quick@wg0` fails to start; `journalctl -u wg-quick@wg0` mentions the `wireguard` module or "Unknown device type" | Kernel module not available (see Kernel section above) | On a real kernel: `modprobe wireguard` manually to see the real error; check `uname -r` is 5.6+; on older kernels install `wireguard-dkms` for your distro. On some container-based VPS: not fixable, contact the provider. |
| Peer added successfully in `wg0.conf` but `wg syncconf` fails | The interface isn't actually up yet (install step didn't fully succeed) | Fix the install issue first (see row above), then re-run "Add WireGuard Customer" — it's safe to re-run, it won't duplicate the peer. |
| Customer's WireGuard client shows "Handshake did not complete" | Cloud firewall blocking the `ListenPort` UDP, or (if going through the carrier) Simorgh's ICMP path isn't actually up | Check the cloud firewall first (see Firewall section). Then check `docker logs simorgh_tunnel` for handshake errors, and confirm `PASSWORD` in the customer's Simorgh carrier launch command matches what's in `/etc/simorgh/customers.json` — a mismatch here fails completely silently by design (see docs/PROTOCOL.md), so double-check it byte-for-byte. |
| Peer connects, handshake succeeds, but no actual internet access through the tunnel | `net.ipv4.ip_forward` not enabled, or the `PostUp` NAT/MASQUERADE rule didn't apply (wrong external interface auto-detected) | `sysctl net.ipv4.ip_forward` should print `1`. Check `iptables -t nat -L POSTROUTING -n` shows a MASQUERADE rule for your actual external interface — if the auto-detected interface in `wg0.conf`'s `PostUp` line is wrong (common on multi-NIC VPS), edit it manually and `systemctl restart wg-quick@wg0`. |
| Two customers' traffic seems to interfere, or one customer can see effects of another's connection/disconnection | This should not happen — Simorgh's multi-client mode isolates every customer's session, FEC, and relay socket independently | This would be a real bug if it happens with the code as shipped — please report the exact `docker logs simorgh_tunnel` output rather than assuming it's expected. |
| A TCP forward-mode (`FORWARD_PROTO=tcp`) connection hangs forever instead of closing when the far end ends the connection | *(fixed)* — earlier versions of the TCP forward sinks swallowed read errors instead of surfacing them, so neither side ever learned the connection had ended | Already fixed: a `pktClose` control packet now propagates a local close to the peer. If you still see hangs, check you're on the current `core/` and not an older build. |
| Per-customer `bandwidth_mbps` in `CUSTOMERS_FILE` doesn't seem to do anything | Traffic isn't actually going through Simorgh's carrier hop for that customer — e.g. they connected straight to WireGuard's public port instead of through their Simorgh carrier client | Confirm the customer's device is actually running the Simorgh carrier client and their VPN client's Endpoint points at `127.0.0.1:<their LOCAL_PORT>`, not the server's real public IP/port directly. |
| `wg_add_customer` reports "already exists" for a name you haven't used | A leftover `[Peer] # customer:<name>` block from a previous run that wasn't fully removed | Check `wg0.conf` for the block and remove it manually (or via "List / Remove WireGuard Customers"), then retry. |

## Server optimization notes

- Running Docker (Simorgh's raw-socket container) alongside WireGuard (a
  kernel-level interface) on the same box is normal and doesn't conflict —
  they operate at different layers and don't compete for the same ports as
  long as `TARGET_PORT` (Simorgh's relay target) matches WireGuard's actual
  `ListenPort`.
- If you enable `BANDWIDTH_LIMIT` on the Simorgh side per customer, remember
  it shapes the **carrier** hop only — WireGuard itself has no bandwidth
  limiting of its own, so this is your only real lever for per-customer
  bandwidth caps in this stack today.
- FEC (`FEC_ENABLE=true`) is recommended when carrying WireGuard traffic
  over the carrier hop, same reasoning as documented in docs/PROTOCOL.md —
  it costs a little bandwidth to meaningfully reduce the retransmit-driven
  latency spikes that hurt WireGuard's own perceived responsiveness on a
  lossy path.

## OpenVPN (built — see nodepanel/)

`protocols/openvpn.sh` installs `openvpn` + `easy-rsa`, manages the PKI,
and supports a shared CA across multiple nodes so one client certificate
is valid everywhere — see `nodepanel/README.md` for the full multi-location
workflow and its testing status (tested more thoroughly than WireGuard was
possible to, including a real server+client connection with actual data
flow, since OpenVPN doesn't need a special kernel module).

Common OpenVPN troubleshooting:

| Symptom | Likely cause | Fix |
|---|---|---|
| `easyrsa` hangs or errors asking for a "Common Name" | Running it without batch mode | Always set `EASYRSA_BATCH=1` for non-interactive use — already done throughout `openvpn.sh`, only relevant if you're scripting easyrsa yourself outside it. |
| Importing a shared CA fails with "Missing expected directory: reqs" (or similar) | easyrsa expects `pki/reqs`, `pki/issued`, `pki/certs_by_serial` to exist even when empty, and `pki/index.txt.attr` to exist | Already handled by `ovpn_install_core`'s import path — if you're building your own CA transfer instead of using it, make sure to create those. |
| `openvpn-server@server` won't start under systemd | Same categories as WireGuard: kernel/module issues are irrelevant here (OpenVPN uses a plain TUN device, not a special module) — check `journalctl -u openvpn-server@server` for the real error, most commonly a malformed `server.conf` or a port already in use | Fix the specific error shown; re-running `ovpn_install_core` is safe. |
| Multi-remote `.ovpn` connects to the wrong node, or none | `<ca>`/`<cert>`/`<key>`/`<tls-auth>` blocks must come *before* the `<connection>` blocks in the file, or OpenVPN warns they're "ignored by previous `<connection>` blocks" | `nodepanel`'s generator already orders it correctly; if hand-building one, keep the shared blocks first. |

## What's next (not built yet)

- L2TP/IPsec installer (`xl2tpd` + `strongswan`, with its own — likely much
  longer — troubleshooting table; L2TP/IPsec is materially more fragile
  than WireGuard/OpenVPN in practice, as seen in this project's own
  debugging session)
- Multi-client support for `MODE=tun` (currently forward-mode only)
- Multi-location for WireGuard (OpenVPN's `<connection>` blocks have no
  WireGuard equivalent — a WireGuard customer wanting failover across
  locations currently needs a separate config per location, or Simorgh's
  own carrier-mode multi-server failover instead)

## Known open item: unlimited customers under extreme local burst

During testing, a customer configured with **no** `bandwidth_mbps` cap was
observed to transfer *more slowly and incompletely* than a capped one, in a
loopback (no real network latency) test where hundreds of packets were sent
back-to-back with no pacing at all. A bandwidth-limited customer doesn't
hit this because the token bucket naturally paces its own sends. This was
only observed over loopback, where nothing spaces packets out the way real
network latency does - it has not been confirmed to occur over an actual
internet path, but hasn't been ruled out either.

**Practical mitigation until this is investigated further**: give every
customer a `bandwidth_mbps` cap (even a generous one, e.g. 50-100), rather
than leaving it fully unset. This costs nothing meaningful for real usage
and avoids the burst pattern that triggered the issue in testing.
