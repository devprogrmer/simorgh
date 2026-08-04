# Configuration Reference

All of these are environment variables read by `simorgh-core` (see
`core/config.go`). The management script (`install.sh`) prompts for the
common ones and writes them to `/etc/simorgh.conf`, then passes them into
the container as `-e` flags.

| Variable | Default | Meaning |
|---|---|---|
| `PASSWORD` | *(required)* | Shared tunnel secret. Authenticates the handshake; never used as the encryption key directly. Must match on both ends. |
| `INTERFACE` | `simorgh0` | Name of the TUN (or TAP, in bridge mode) device. Only used when `MODE=tun`. |
| `MODE` | `tun` | `tun` (Simorgh is the full VPN, own TUN device) or `forward` (Simorgh only carries another VPN's UDP traffic - see docs/PROTOCOL.md). |
| `FORWARD_BIND` | `127.0.0.1` | `forward` mode, client side: local address to listen on. Point your VPN client's Endpoint here. |
| `LOCAL_PORT` | `51000` | `forward` mode, client side: local port to listen on. |
| `TARGET_HOST` | `127.0.0.1` | `forward` mode, server side: where your real VPN server is listening. |
| `TARGET_PORT` | `51820` | `forward` mode, server side: the real VPN server's port. |
| `FORWARD_PROTO` | `udp` | `forward` mode: `udp` or `tcp` — must match the real VPN's own transport. See docs/PROTOCOL.md for the TCP-mode correctness caveat. |
| `CUSTOMERS_FILE` | *(unset)* | Server + `forward` mode only. Path to a JSON list of `{name, password, bandwidth_mbps}` entries (`bandwidth_mbps` optional) — switches the server into multi-client mode, one isolated session per customer, each with its own optional Mbps cap. See docs/DEPLOYMENT.md. |
| `TRANSPORT` | `icmp` | `icmp`, `udp`, or `auto` (client only — tries icmp then udp, locks onto whichever works). Both concrete transports are dual-stack (IPv4 + IPv6) automatically. |
| `REMOTE_IP` | *(unset)* | Set only on the client (IRAN) side. Single host, or comma-separated list (e.g. `1.2.3.4,5.6.7.8`) for automatic failover to whichever server currently measures best. Its presence selects the client role. |
| `PING_INTERVAL` | `1` | Seconds between live RTT/loss probes on the active session (client only). |
| `HEALTH_CHECK_INTERVAL` | `5` | Seconds between probing every *other* configured server, when 2+ are set (client only). |
| `SWITCH_MARGIN_MS` | `15` | A candidate server must beat the active path's score by at least this much to be considered (client only). |
| `SWITCH_CONFIRM_ROUNDS` | `3` | Consecutive health checks a candidate must win before the client actually switches to it (client only). |
| `UDP_PORT` | `51900` | Port used when `TRANSPORT=udp`. Must match on both ends. |
| `MAC` | *(unset)* | Optional custom MAC address, only meaningful in TAP/bridge mode. |
| `MTU` | `1400` | Interface MTU. Use the MTU Optimizer in the management script to find the largest value that doesn't fragment on your path. |
| `KEEPALIVE` | `5` | Heartbeat interval in seconds. Also controls how long the client waits (6× this) before re-handshaking after silence. |
| `LINK_QUALITY` | `0` (off) | Soft warning threshold (0–100 %). Logs a warning when estimated quality drops below it. |
| `BAN_QUALITY` | `0` (off) | Hard threshold (0–100 %). Below it, the process exits to force a clean reconnect via the container's restart policy. |
| `DSCP_MARK` | `-1` (off) | DSCP value (0–63) to mark on outgoing packets. Client + ICMP transport only. |
| `BANDWIDTH_LIMIT` | *(unset, unlimited)* | Server-side downstream cap in Mbps. |
| `FEC_ENABLE` | `false` | Enable lightweight XOR parity for single-packet-loss recovery per group. |
| `FEC_GROUP` | `8` | Base number of data packets per parity packet (2–64). Adapts automatically (4/8/16) once the peer starts reporting observed quality via keepalives - see docs/PROTOCOL.md. |
| `OPERATING_MODE` | *(empty)* | See below. |

## OPERATING_MODE

- *(empty)* — the manager script assigns the tunnel IP itself via
  `ip addr add`, as configured in `/etc/simorgh.conf` (`LOCAL_TUN_IP` /
  `TUN_MASK`).
- `ip:mask:srv_ip:cli_ip:dynamic:metric` — the core assigns the address
  itself: `srv_ip` on the FOREIGN side, `cli_ip` on the IRAN side, and
  installs a route to the peer with the given `metric` if provided.
  `dynamic` is reserved for a future DHCP-style mode and is currently a
  documented no-op.
- `bridge:br0:br1` — the interface is created as a **TAP** (not TUN) and
  enslaved to `br0` (server side) or `br1` (client side). The named bridge
  must already exist on the host.

## Capabilities the container needs

`install.sh` always requests both:

- `--cap-add=NET_ADMIN` — interface and route configuration.
- `--cap-add=NET_RAW` — required for the ICMP transport's raw socket
  (harmless if unused, i.e. when `TRANSPORT=udp`).

Plus `--device /dev/net/tun:/dev/net/tun` and `--net=host` (so the TUN
device it creates lands directly in the host's network namespace).
