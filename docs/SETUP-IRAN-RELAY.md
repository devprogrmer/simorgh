# Setup: Iran relay to one or more servers abroad

The common topology, and the one most people mean when they set this up:

```
Customer (Tehran) → Iran server → Simorgh tunnel → Foreign server → Internet
```

Customers connect to an Iranian address. That first hop is local and fast, which
is the entire reason the Iranian server is in the path. The tunnel carries their
traffic abroad.

If instead you want customers connecting **straight** to each foreign server and
picking a country in their app, you want the direct topology — skip to
[Direct multi-location](#direct-multi-location) at the end.

## What runs where

| | Iran server | Foreign server(s) |
|---|---|---|
| Panel | yes | no |
| Simorgh tunnel | client | server |
| The actual VPN daemon (WireGuard, OpenVPN…) | no | yes |
| Panel node | optional | optional |

The VPN daemon runs **abroad**. The Iranian server only relays. That is what
forward mode is for.

## 1. Foreign server — tunnel server

In the panel: **Settings → Tunnel**, or set these directly.

| Setting | Value |
|---|---|
| Mode | `forward` |
| Operating mode | `server` |
| Password | anything long; must match the Iranian side exactly |
| Transport | `icmp` |
| Target host | `127.0.0.1` |
| Target port | the real VPN daemon's port, e.g. `51820` for WireGuard |

Then start the VPN daemon itself on this machine as you normally would — it
listens on localhost and never faces the internet directly.

## 2. Iran server — tunnel client

| Setting | Value |
|---|---|
| Mode | `forward` |
| Operating mode | `client` |
| Password | the same one |
| Transport | `icmp` |
| Remote IP | every foreign server, comma separated |
| **Forward bind** | **`0.0.0.0`** |
| Local port | what customers will connect to, e.g. `51820` |

### Forward bind is the one that catches people

It defaults to `127.0.0.1`, which means the tunnel is reachable only from the
Iranian server itself. Every customer connection is refused and nothing in the
logs says why, because as far as the tunnel is concerned nothing arrived. Set it
to `0.0.0.0`.

### Several foreign servers

Put them all in Remote IP, comma separated:

```
203.0.113.10,198.51.100.7,192.0.2.44
```

The core measures real RTT and packet loss on each one continuously and moves to
whichever is genuinely best, with hysteresis so it does not flap. **This is the
failover** — there is no separate switch to turn on, and no manual picking.

## 3. Give customers the Iranian address

This is the step that is easy to get wrong, and getting it wrong silently
bypasses everything above.

The VPN daemon runs abroad, so the panel manages it on the foreign node. But
customers must dial **Iran**. If their config names the foreign server, their
traffic goes straight there and the Iranian server — the whole reason for the
low latency — is never used.

So on the placement, set **Advertise** to the Iranian server's public address.

- **Listen** — where the daemon binds, on the machine it runs on.
- **Advertise** — what the customer's config says. Leave empty for direct
  connection; set it when a relay sits in front.

With Advertise set, the subscription hands out the Iranian address while the
panel keeps managing the daemon abroad.

## 4. Check it

1. From your own machine, connect a client using the subscription.
2. Confirm the config's endpoint is the **Iranian** address.
3. Check the exit IP is the **foreign** server's.

If the endpoint is right and the exit IP is Iranian, the tunnel is not carrying
the traffic — check Forward bind and that both sides share a password. If you
cannot connect at all, try `udp` transport instead of `icmp`; some paths drop
ICMP entirely.

## Do the foreign servers need to be panel nodes?

**No — not for this topology.** Traffic reaches them through the tunnel whether
the panel knows them or not.

Making them nodes buys you remote management: status, per-core logs, and
automatic prerequisite installation from the panel instead of over SSH. That is
genuinely useful, but it is convenience, not connectivity.

It also adds a dependency worth understanding. The panel dials each node on its
API port (62050 by default) — an outbound connection from Iran to an unusual
port, which is exactly the kind of thing that gets filtered. If that channel is
blocked, the node shows **offline** and you can no longer manage it from the
panel, but **customer traffic keeps flowing**: offline means the control channel,
not the data path, and the daemons carry on serving what they last had.

**Get the tunnel working first without nodes.** Add them afterwards if you want
remote management. If the control channel turns out to be filtered, you have lost
nothing.

## Direct multi-location

If you would rather customers connect straight to each foreign server:

- Place the inbound on each foreign node.
- Leave **Advertise** empty, so each config names its own node.
- No tunnel needed.

The subscription then carries one config per location, labelled with the node
name, and the customer picks. All of them bill against the one quota on their
account.

Higher latency than the relay, but no Iranian server in the path to fail, and
nothing to filter between the customer and the exit.
