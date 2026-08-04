# Simorgh Node Panel

A small, standalone web UI for creating multi-location WireGuard customer
configs without manual SSH — the practical alternative to a full
Marzban-style "Node" feature (see `docs/DEPLOYMENT.md` for why that wasn't
built directly into `panel/`).

## What it is, and isn't

- It is a thin web front-end over SSH: when you click "Create," it SSHes
  into the node(s) you picked and runs the relevant protocol function
  there — the exact same functions tested elsewhere in this project, just
  invoked remotely.
- It does **not** install anything new on a node beyond what the bootstrap
  flow itself installs (see below) — no separate manual setup needed.
- **Two protocols**: WireGuard (single location per config) and OpenVPN
  (one config can span multiple locations — see below).
- Generated configs are **direct** connections — no Simorgh carrier
  wrapping, no second piece of software the customer has to run.
- **Per-customer bandwidth limiting is not wired up in this direct mode.**
  Simorgh's token-bucket limiter needs traffic to actually flow through a
  Simorgh process to enforce anything — a direct connection never touches
  Simorgh at all. If you need bandwidth caps *and* multi-location, use the
  carrier-mode output from `wireguard.sh` directly instead of this tool.

## OpenVPN: two ways to hand out multiple locations

Both share the same underlying mechanism (one client certificate, valid on
every node because they share a CA) but differ in what the customer
experiences:

- **Separate file per location** (default) — pick N nodes, get back N
  standalone `.ovpn` files (e.g. `dave-turkey.ovpn`, `dave-uae.ovpn`,
  `dave-armenia.ovpn`), each a complete, independent config. The customer
  imports whichever one(s) they want and picks between them as separate
  profiles in their client. This is what most people mean by "pick a
  country" - the customer chooses, not the software.
- **One file, automatic failover** — pick N nodes, get back a single
  `.ovpn` with a `<connection>` block per node in the order you checked
  them. The client always tries the first one first and only moves to the
  next automatically if it's unreachable - there's no user choice involved,
  it's pure reliability/failover, and the order is fixed by the file, not
  picked per-connection by the customer.

Choose per-location for "let the customer pick a country themselves,"
failover for "just keep them connected to *something*."

For one client certificate to be valid on every node either way, they all
need to trust the same CA. The bootstrap flow handles this automatically:
the *first* OpenVPN node you bootstrap generates a fresh CA; every node
after that has the first one's CA exported and imported into it before its
own install runs. You don't do anything differently — just bootstrap each
node the same way, in any order after the first.

## Adding a node — two ways

**Bootstrap a brand-new server** (just give it SSH host + username + password):
the panel installs its own SSH key on the server (so the password is never
stored or reused again), pushes `wireguard.sh`, installs `wireguard-tools`,
runs `wg_install_core` non-interactively with the port/subnet you specify,
and registers the node — all from one form submission.

**Register an already-set-up node**: if you'd rather set up SSH keys and
run `install.sh` yourself first (the original flow), there's a second form
for that — just host/user/port, no password needed since your key is
already trusted.

Both end up in the same place: a node you can create customers on.

## Requirements

- For **bootstrap**: SSH access with a username and password to a fresh
  server (root, since installing WireGuard needs root either way — the
  same requirement `install.sh` itself has).
- For **registering an existing node**: SSH key access already set up
  (`ssh-copy-id`) from wherever you run this panel.
- Either way, once a node is registered, all *ongoing* operations
  (creating customers) use a dedicated key this panel generates for itself
  at `/etc/simorgh/nodepanel_id_ed25519` — never the node's password.
- `sshpass` must be installed wherever this panel runs (`apt install
  sshpass`) — used only for the one-time password step during bootstrap.

## Running it

```bash
cd nodepanel
go build -o simorgh-nodepanel .
NODEPANEL_LISTEN=127.0.0.1:8787 ./simorgh-nodepanel
```

Bind it to `127.0.0.1` and reach it over an SSH tunnel or your own reverse
proxy with auth in front — there is no login built into this tool itself;
it's meant to sit behind something else if exposed beyond localhost.

`NODEPANEL_DATA` (default `/etc/simorgh/nodepanel.json`) is where the node
list is persisted — plain JSON, `{name, host, ssh_user, ssh_port}` per
entry. The node's password is never part of that file, or written
anywhere, at any point.

## Testing status

**WireGuard**, tested end-to-end for real:
- **Bootstrap**: a local SSH server standing in for a fresh node, password
  auth, a full bootstrap run through the web form. Confirmed: the key got
  installed, `wireguard.sh` was actually pushed and present on the "node"
  afterward, and `wg0.conf` was generated with exactly the port/subnet
  submitted in the form.
- **Ongoing management**: created a customer on that same bootstrapped
  node afterward using *only* the installed key (no password given) and
  got back a correct WireGuard config with the right subnet-derived IP and
  port.

**OpenVPN**, tested more thoroughly (it doesn't need a special kernel
module the way WireGuard does, so a real server+client connection could
actually be brought up in the build environment):
- A real OpenVPN server was started from a config this module generated,
  and reached "Initialization Sequence Completed."
- A real client, using a cert this module issued, completed a full TLS
  handshake and certificate verification, and **exchanged real ping
  traffic through the tunnel at 0% packet loss**.
- The shared-CA mechanism was verified directly: two independently
  bootstrapped "nodes" ended up with byte-identical CA certificates after
  export/import, and a client cert issued against the shared CA was
  **trusted by a second, independent server process** using that same CA.
- Multi-remote failover was verified directly: with a two-`<connection>`
  config, killing the first node's server process caused the client to
  automatically fail over to the second and resume real ping traffic.
- Through `nodepanel`'s actual HTTP+SSH code path specifically: a full
  password bootstrap of a real OpenVPN node (key install, script push, PKI
  generation) and a full customer-creation call that performed a real SSH
  round trip, correctly parsed the ca/cert/key/tls-auth PEM blocks out of
  the live command output, and assembled them into a correctly-ordered
  combined `.ovpn`. The **separate-file-per-location** mode was also
  tested through the same real HTTP+SSH path and confirmed to produce a
  correctly-labeled, standalone single-remote file.
- **What wasn't retested together**: bootstrapping two *separately
  registered* `nodepanel` nodes and then creating one customer spanning
  both, in a single run through the actual web UI. The sandbox has one
  filesystem, so two "different nodes" bootstrapped through `nodepanel`
  would collide on the same `/etc/openvpn/server` path. The two halves
  that make this work — CA sharing (tested directly, above) and
  `nodepanel`'s multi-block combiner (tested directly, above) — were each
  verified independently instead. Confirm the full multi-node flow on your
  actual servers before relying on it for real customers.

In every case, `wg-quick@wg0` / `openvpn-server@server` failing to start
under `systemctl` was the same known, pre-existing sandbox limitation (no
real WireGuard kernel module, no systemd as PID 1) — not a new issue.
Everything up to and including live data flow (for OpenVPN) or correct
config generation (for WireGuard) was verified working for real.
