# Simorgh Core — Wire Protocol

## Modes: full tunnel vs. carrier

`MODE=tun` (default) is everything described above: Simorgh owns a TUN
device and is the actual VPN — its own AES-256-GCM session *is* your
traffic's encryption.

`MODE=forward` is different on purpose: Simorgh does **not** touch your
VPN's own encryption. It relays another VPN's (WireGuard, OpenVPN, anything
UDP-based) already-encrypted packets between a local port and a remote
target, using everything above - handshake, session AEAD, FEC, link
quality - purely as the *carrier*. You still get:

- your real VPN's own, independently-audited cryptography, untouched
- Simorgh's low ICMP overhead and packet-loss recovery for that carrier hop
- a second, independent layer of encryption around the already-encrypted
  payload (defense in depth, and it also means an observer sees ICMP
  traffic, not a recognizable WireGuard/OpenVPN handshake)

In `forward` mode, no TUN device is created at all:

- **IRAN (client) side**: Simorgh listens on `FORWARD_BIND:LOCAL_PORT`.
  Point your VPN client's `Endpoint` (WireGuard) or `remote` (OpenVPN) at
  that address instead of the real server.
- **FOREIGN (server) side**: Simorgh relays decrypted-at-the-carrier-layer
  payloads to `TARGET_HOST:TARGET_PORT`, where your real VPN server is
  listening (normally `127.0.0.1` plus its usual port).

`FORWARD_PROTO` selects which of the real VPN's own transports gets
carried:

- `udp` (default) — WireGuard, OpenVPN-UDP, L2TP/IPSec, and most modern
  protocols. Datagram-shaped, matches how the underlying tunnel already
  works: each local UDP datagram becomes one tunnel frame.
- `tcp` — OpenVPN-TCP, Cisco AnyConnect/IPSec-over-TCP, Xray/VLESS-TCP,
  Trojan, and other TCP-based protocols. Simorgh terminates a real TCP
  connection locally and a separate real TCP connection to the target, and
  relays bytes between them through the tunnel. **This is not a fully
  reliable relay**: an uncorrected loss inside the tunnel corrupts the byte
  stream, and the affected connection has to reconnect — the same as it
  would over any unreliable link. Enable `FEC_ENABLE` for anything beyond
  casual use of TCP forward mode. When either side's local TCP leg closes
  (the target service ends the connection, or the local client disconnects),
  a `pktClose` control packet tells the peer to close its matching leg too,
  rather than leaving it hanging on a read that will never get data.

This is a carrier, not a security claim: Simorgh has not been independently
audited the way WireGuard's Noise-based protocol has. Use `forward` mode
when you want WireGuard/OpenVPN's proven security with a lower-overhead,
ICMP-shaped path underneath it; use `tun` mode only when you specifically
want Simorgh's own encryption to be the VPN.

## Envelope

Every packet, regardless of transport, carries the same 4-byte envelope
header in front of its body:

```
[ sessID  uint16 ][ seq  uint16 ][ body... ]
```

- **sessID**: derived from `SHA256("simorgh-session-id|" + password)`.
  Both peers compute it independently before any handshake happens, and use
  it as a cheap first-pass filter to ignore unrelated traffic (background
  internet noise, the other side's own kernel echoing packets back, etc.)
  before spending CPU on anything cryptographic.
- **seq**: a per-sender monotonic counter, used only for link-quality
  estimation (rolling loss tracking). It is not a security boundary.

## Transports

- **ICMP** (default): the envelope is carried as the payload of an ICMP
  **echo request** (type 8/IPv4, type 128/IPv6). Only echo *requests* are
  ever treated as data — echo *replies*, including each host's own kernel
  auto-replying to the other side's requests, are ignored outright. This
  sidesteps the self-feedback loop that would otherwise occur.
  **Dual-stack**: the core opens both an IPv4 raw socket and an IPv6
  ICMPv6 raw socket where available, and merges their traffic. `REMOTE_IP`
  can be an IPv4 address, an IPv6 address, or a mix of both in a
  multi-server list — whichever family a given host resolves to is used
  automatically. If one family is unavailable on the host (no IPv6 route,
  IPv6 disabled, etc.), the core logs it and falls back to the other,
  rather than failing outright.
- **UDP** (fallback): the envelope is a plain UDP datagram. Also
  dual-stack, via a single wildcard-bound socket that accepts both
  families on Linux.
- **`TRANSPORT=auto`** (client only): tries ICMP first, then UDP, against
  every configured server, and locks onto whichever one actually gets a
  handshake reply — the client finds a working path itself rather than the
  operator having to guess and hardcode one. Requires the server side to
  actually be reachable on whichever transport wins; see
  docs/DEPLOYMENT.md for how to run both simultaneously.

## Handshake (X25519)

Before any data flows, the two peers perform a handshake to derive a fresh,
forward-secret session key. The password is used only to *authenticate*
this handshake via HMAC-SHA256 — never as an encryption key.

```
client -> server:  HELLO      = clientPub(32) || HMAC(handshakeKey, clientPub)
server -> client:  HELLO_ACK  = serverPub(32) || HMAC(handshakeKey, serverPub || clientPub)
```

- `handshakeKey = SHA256("simorgh-handshake|" + password)`
- Both sides then compute the same shared secret via X25519 ECDH, and derive
  the session key as:

```
sessionKey = SHA256("simorgh-session-key|" + sharedSecret || <pubkeys, canonical order>)
```

That session key is used to build an **AES-256-GCM** AEAD cipher for
everything that follows. Compromising one session's key (or even the
long-term password, after the fact) does not expose *past* sessions'
traffic, since the session key itself is never transmitted.

The client re-initiates a handshake whenever it hasn't heard from the
server in `6 × KEEPALIVE` seconds. The server accepts a fresh Hello at any
time and simply replaces its current session — this is what lets the client
reconnect after a restart without needing the server restarted too.

## Data channel

Once a session exists, every envelope body is `nonce(12) || AEAD-ciphertext`.
The decrypted plaintext's first byte is the packet type:

| Byte | Meaning |
|---|---|
| `0x01` | Data — `[groupID uint32][index uint8][groupSize uint8][frame...]` |
| `0x02` | Keepalive — `[observedQualityPct uint8]` (how clean *incoming* traffic looked to the sender - see Adaptive FEC) |
| `0x03` | FEC parity — `[groupID uint32][groupSize uint8][parity bytes]` |
| `0x04` | Ping — `[8-byte token]`, a live RTT probe on an established session |
| `0x05` | Pong — `[8-byte token]`, echo of the same token |
| `0x06` | Close — no body. "My local TCP leg just ended, close yours too." Only used by `FORWARD_PROTO=tcp`; see below. |

`groupSize == 0` on a data packet means FEC is not in use; the frame is
delivered to the TUN device (or forward-mode relay) immediately either way.

## Live RTT/jitter measurement (ping/pong)

Independent of data traffic, the client pings the currently active session
every `PING_INTERVAL` seconds (default 1s) and measures the round trip when
the peer echoes it back as a pong. This gives a real, continuously-updated
RTT figure - and, via missed pongs, a loss estimate - even when the tunnel
is otherwise idle. This is what multi-server failover (below) scores paths
on, and it runs whether or not you have more than one server configured.

## FEC (optional, adaptive)

When `FEC_ENABLE=true`, the sender groups outgoing frames (`FEC_GROUP` by
default, but see below), XORs a length-prefixed, zero-padded copy of each
into a running parity accumulator, and sends that parity as one extra
packet once the group is full. Data packets are **never delayed** for this
— parity is purely insurance sent after the fact. If the receiver is
missing exactly one frame from a group when its parity arrives, it recovers
the missing frame by XORing the parity against everything else it received
— no retransmit round trip required. Two or more losses in the same group
are not recoverable and are simply dropped, same as without FEC.

**Adaptive sizing**: every keepalive carries a byte reporting how clean the
*sender's* incoming traffic has looked (its own `linkQuality` estimate).
The peer reads that and adjusts its own outgoing FEC group size in
response - noisier reported quality means smaller groups (more frequent,
cheaper parity, faster recovery), a clean reported link means larger groups
(less overhead). `FEC_GROUP` is the size used until the first quality
report arrives, and is clamped to 4/8/16 by the observed quality band
afterward.

## Link quality

Each receiver estimates loss by tracking gaps in the peer's `seq` counter
over a rolling ~200-packet window:

- `LINK_QUALITY` (0–100, 0 = off): logs a warning below this percentage.
- `BAN_QUALITY` (0–100, 0 = off): the process exits below this percentage,
  so the container's restart policy forces a clean reconnect rather than
  limping along on a bad path.

## Multi-server auto-failover (client only)

`REMOTE_IP` may be a single host or a comma-separated list. With more than
one:

- The client connects to the first one that answers on startup.
- Every `HEALTH_CHECK_INTERVAL` seconds, it probes every *other* configured
  server with a real (but disposable) X25519 handshake, measuring genuine
  round-trip time over the real network path - not a synthetic estimate.
- Each path is scored as `rttMs + lossPct * 20` (loss is weighted heavily,
  since it costs a gaming connection far more than a few extra
  milliseconds). The active path's score comes from the live ping/pong
  stats above.
- A candidate must beat the active path's score by at least `SWITCH_MARGIN_MS`
  for `SWITCH_CONFIRM_ROUNDS` consecutive checks before the client switches
  - this hysteresis stops it flapping between two similar paths.
- On switch, a fresh handshake is performed against the winning server (the
  probe handshake is discarded, not reused, so forward secrecy is
  unaffected) and it becomes the new active session.

This only activates with 2+ servers configured; with one, none of this
machinery runs and the client behaves exactly as if it didn't exist.

## DSCP marking (client, ICMP transport only)

When `DSCP_MARK` (0–63) is set, the client sets `IP_TOS` on its raw socket
so every outgoing ICMP packet carries that DSCP value.

## Bandwidth limiting (server only)

When `BANDWIDTH_LIMIT` (Mbps) is set, the server's outbound path is shaped
through a token bucket with a 1-second burst allowance, to avoid
bufferbloat (which itself adds latency).

## Multi-client server mode

By default the server (both `tun` and `forward` modes) serves exactly one
active session, authenticated by the single `PASSWORD`. Setting
`CUSTOMERS_FILE` on the server (forward mode only, currently) switches it
to multi-client: a JSON list of `{name, password, bandwidth_mbps}` entries
(`bandwidth_mbps` optional, 0/omitted = unlimited), each served as a
**fully isolated** session — its own session crypto, FEC state, link-quality
tracker, relay socket to `TARGET_HOST:TARGET_PORT`, and bandwidth cap. No
protocol change was needed for this: each customer's distinct password
already produces a distinct `sessID` (see the envelope section above),
which is what the server demultiplexes incoming traffic on.

The per-customer `bandwidth_mbps` cap is a real, working rate limit
(the same token-bucket limiter as the single-customer `BANDWIDTH_LIMIT`
env var, just scoped per customer) - this was added specifically because
neither WireGuard nor the 3X-UI/vpn-ui panel family have a working
equivalent as of this writing (multiple long-open, unresolved upstream
feature requests confirm the gap - see docs/DEPLOYMENT.md for sourcing).
It was verified with a real throughput test: a customer capped at 2 Mbps
measured at 2.53 Mbps actual (the small excess is the token bucket's
1-second burst allowance, not a limiter failure).

This is what `protocols/wireguard.sh` uses to give each WireGuard customer
their own carrier session sharing one Simorgh server process. See
`docs/DEPLOYMENT.md` for the full setup.

## Required capabilities

- `CAP_NET_ADMIN` — interface/route configuration.
- `CAP_NET_RAW` — required for the ICMP transport's raw socket. Harmless
  (unused) if `TRANSPORT=udp`.
