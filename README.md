# Simorgh — Gaming Tunnel

Simorgh is a self-hosted, low-latency tunnel purpose-built for routing game
(and general VPN) traffic between a server in Iran and a server abroad —
minimizing ping and recovering from packet loss without adding round-trip
delay. Everything runs from a Docker image **you build yourself**. No
tunnel image is ever pulled from any registry — `core/` is the full Go
source, and `install.sh` runs `docker build` against it.

This repo also includes: protocol installers for **WireGuard** and
**OpenVPN**, a small **multi-location web panel** (`nodepanel/`) for
managing them without manual SSH, and a rebranded fork of the **vpn-ui**
panel (`panel/`) for Xray/VMess/VLESS/Trojan/L2TP/etc.

## Quick install

**One-liner (downloads and runs `install.sh`):**

```bash
curl -fsSL https://raw.githubusercontent.com/devprogrmer/simorgh/main/install.sh -o /tmp/simorgh-install.sh && sudo bash /tmp/simorgh-install.sh
```

This downloads the script to a real file first, then runs it — safer than
piping straight into `bash`, and it means `install.sh` has a real `$0` to
work with. On first run it clones the rest of the repo automatically (it
needs `core/` and `protocols/`, which aren't in the single script file) —
you don't need to do anything extra.

**Full clone (if you want to read the code first, which is a reasonable
thing to want before running anything as root):**

```bash
git clone https://github.com/devprogrmer/simorgh.git
cd simorgh
chmod +x install.sh
sudo ./install.sh
```

Both paths end up at the same interactive menu. Run this on **both**
servers (Iran and abroad).

## Recommended setup — step by step

This is the order that makes sense for most people, gaming-focused or not.

### 1. Decide what you actually want

| You want... | Use |
|---|---|
| A single low-latency tunnel, no third-party protocol | `MODE=tun` (Simorgh is the whole VPN) |
| Lower ping for a WireGuard/OpenVPN/etc. connection you already run | `MODE=forward` (Simorgh only carries it) |
| To sell/hand out VPN access to multiple people | Protocol installers + `nodepanel/` (see below) |

If you're not sure: start with `MODE=tun` on one Iran + one foreign server,
get it working, then decide if you need more.

### 2. On both servers: Install Core, then Create Tunnel

From the menu:
1. **Install Core** — builds `simorgh-core:latest` locally via
   `docker build`. Installs Docker first if it's missing. Nothing is
   pulled from any tunnel-image registry, ever.
2. **Create Tunnel** — pick `IRAN server` on the Iran box, `FOREIGN server`
   on the abroad box. Use the **same password and transport** on both.

Defaults that work well for gaming, and why:

- **Transport**: leave it on `icmp` first (lowest overhead, least likely to
  be filtered). If it won't connect, recreate the tunnel with `udp`
  instead, or use `auto` on the client side once you've confirmed the
  server can be reached on both (see `docs/DEPLOYMENT.md`).
- **FEC**: turn it on (`y`) if the path has any packet loss at all — it
  recovers a lost packet without waiting for a retransmit, which is
  exactly the kind of latency spike that hurts gaming most. It adapts its
  own overhead automatically, so there's little downside to leaving it on.
- **MTU**: leave the default, then run **MTU Optimizer** (menu option 5)
  *after* the tunnel is up and connected — it needs a live path to test
  against.
- **Multiple foreign servers**: if you have more than one candidate
  foreign server, give the client a comma-separated list
  (`1.2.3.4,5.6.7.8`) instead of one IP. It continuously measures real
  RTT/loss on each and switches to whichever is genuinely best — no
  manual picking needed for this use case.

### 3. Confirm it actually works

Dashboard (menu option 4) shows live status. If something's wrong, check
`docs/PROTOCOL.md` for how the handshake/transport is supposed to behave,
and `docs/DEPLOYMENT.md` for known failure modes and fixes.

### 4. If you're serving other people (not just yourself)

This is where it branches from "one tunnel for me" to "a service for
customers":

- **Want to carry WireGuard or OpenVPN, one location, no panel UI?** →
  `protocols/wireguard.sh` / `protocols/openvpn.sh`, driven from the same
  `install.sh` menu (options 7–9 for WireGuard).
- **Want a web UI to add customers/locations without SSHing in every
  time, across WireGuard and/or OpenVPN, possibly several countries?** →
  `nodepanel/`. Bootstrap a node with just its IP + SSH password (it
  installs everything and registers itself), then create customers from
  the browser. OpenVPN supports genuine multi-location subscriptions:
  either one file with automatic failover, or separate per-country files
  the customer picks between themselves. See `nodepanel/README.md`.
- **Want Xray/VMess/VLESS/Trojan/L2TP/IPsec, subscription links, traffic
  quotas, or reselling to sub-accounts?** → `panel/` (the vpn-ui fork).
  **Read `panel/SIMORGH-FORK-NOTICE.md` first** — it has not been
  build-tested in this project's own environment, and it documents a real,
  fairly sophisticated reseller system that was already there upstream
  (traffic-balance ledger, per-reseller quotas, scoped inbound access).

None of these four are mutually exclusive — a common real setup is `panel/`
for Xray-based customers on one server, `nodepanel/` for a couple of
WireGuard/OpenVPN locations, and plain `MODE=tun` Simorgh for your own
personal low-latency link.

## What's inside

- **Transports**: ICMP (default — lowest overhead, least likely to be
  filtered), UDP (fallback), or `auto` (client-only — tries ICMP then UDP,
  locks onto whichever reaches the server). All dual-stack (IPv4 + IPv6).
- **Carrier mode for any protocol**: `MODE=forward` relays UDP-based
  (WireGuard, OpenVPN-UDP, L2TP/IPSec) or TCP-based (OpenVPN-TCP, Cisco
  AnyConnect/IPSec-over-TCP, Xray/VLESS-TCP, Trojan) traffic — pick with
  `FORWARD_PROTO`.
- **Multi-client server**: `CUSTOMERS_FILE` serves many customers from one
  server process, each fully isolated (own crypto, FEC, quality tracking,
  relay socket, optional per-customer bandwidth cap). Forward-mode only.
- **Protocol installers**: `protocols/wireguard.sh` and
  `protocols/openvpn.sh` — real `wireguard-tools`/`openvpn`+`easy-rsa`
  installs, not reimplementations. OpenVPN supports a shared CA across
  multiple nodes for multi-location subscriptions.
- **`nodepanel/`**: web UI for the above, with SSH-password bootstrap (no
  manual server prep) and multi-location OpenVPN generation.
- **Security**: X25519 handshake for forward secrecy + AES-256-GCM session
  encryption. The password authenticates the handshake; it's never the
  encryption key directly, and a fresh key is negotiated every session.
- **Adaptive FEC**, **multi-server auto-failover** (RTT+loss scored, with
  hysteresis), **live link-quality monitoring**, **MTU optimizer**, **DSCP
  marking**, **bandwidth shaping** — see `docs/PROTOCOL.md` for exactly how
  each works.

Go standard library only in `core/` and `nodepanel/` — no third-party Go
modules, so building never depends on reaching any package registry beyond
what Docker Hub's own `golang:alpine` base image needs.

## Requirements

- A Linux VPS in Iran and a Linux VPS abroad, both under your control, with
  either ICMP or a UDP port open between them.
- Docker (installed automatically by `install.sh` if missing).
- Root access on both boxes.

## Testing status — read this before trusting any of it blindly

- **Core tunnel** (both transports, both modes, multi-server failover, TCP
  and UDP carrier relaying, multi-client isolation): exercised end-to-end
  in real namespace-based tests — handshake, data flow, and failover all
  verified working. IPv6 was code-reviewed but **could not be
  runtime-tested** (no IPv6 stack in the build environment) — verify on
  your real servers.
- **WireGuard installer**: config-generation logic tested directly
  (keys, `wg0.conf`, peer add/list/remove). The actual `wg-quick@wg0`
  service start **could not be verified** (no loadable WireGuard kernel
  module, no systemd as PID 1 in the build environment).
- **OpenVPN installer**: tested more thoroughly, since it doesn't need a
  special kernel module — a real server and real client were brought up,
  completed a full TLS handshake, and **exchanged real traffic**. The
  shared-CA multi-node mechanism and multi-remote failover were also each
  verified directly with real, independent server processes.
- **`nodepanel/`**: the full SSH-password bootstrap → prerequisite install
  → customer creation pipeline was tested through the real HTTP+SSH code
  path for both WireGuard and OpenVPN, including the separate-file and
  combined-failover output modes.
- **`panel/`** (vpn-ui fork): **now builds and its test suite passes** — 15
  packages, no failures, on Go 1.26.5. It did not build at all before: two
  `.gitkeep` placeholders the `go:embed` directives depend on were missing, so
  nothing in the panel compiled. That, and a stale test that was the suite's
  only failure, are fixed.
- **Multi-node** (`panel/node/`, `docs/NODES.md`): the panel can now run its
  protocols on remote servers. Hashing, the certificate authority, the
  local/remote conformance suite, replay refusal, cross-node quota, relay
  de-duplication and the route permissions are all covered by tests that were
  run. **Not verified**: the real SSH bootstrap against a real machine, remote
  provisioning end to end, and anything needing a browser. See
  `docs/NODES.md` for the full breakdown.

Where something couldn't be verified, that's stated plainly in the
relevant doc rather than assumed to work — check `docs/DEPLOYMENT.md` and
each component's own README for the specifics.

## Documentation

- `docs/PROTOCOL.md` — wire format, handshake, encryption, FEC, link
  quality, multi-server failover, all config knobs.
- `docs/CONFIGURATION.md` — every environment variable the core reads.
- `docs/DEPLOYMENT.md` — prerequisites, server optimization, real
  troubleshooting tables for WireGuard and OpenVPN, and the reasoning
  behind what was and wasn't built for multi-node/reseller support.
- `docs/NODES.md` — running the panel's protocols on remote servers: adding a
  node, automatic prerequisite installation, what happens when one goes down,
  the security model and its limits. Persian: `docs/NODES.fa.md`.
- `nodepanel/README.md` — the multi-location web panel.
- `panel/SIMORGH-FORK-NOTICE.md` — what changed in the vpn-ui fork, its
  reseller system, and its testing status.

## License

`core/`, `nodepanel/`, `protocols/`, and `install.sh` are **MIT** — see
`LICENSE`. `panel/` is a separate fork under **GPLv3** — see
`panel/LICENSE`. Don't assume MIT terms apply there.
