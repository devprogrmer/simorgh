# Nodes

One panel, many machines. The panel runs in Iran; the nodes run abroad and serve
the actual protocols. Every protocol the panel supports works on a node, because
a node runs the same binary.

Persian: [NODES.fa.md](NODES.fa.md).

## What a node is

A node is a Linux server running `simorgh-panel node`. It has no web UI, no
users and no jobs of its own. The panel pushes it a *desired state* — which
inbounds to serve, with which settings — and the node makes itself match.

It does keep a local database, and that is worth being precise about. Every
protocol driver reads its inbounds through the shared database layer, so a node
without one could not run any of them. What that database holds is a working
copy of what the node must currently serve. What it does **not** hold: the
reseller ledger, admin accounts, traffic history, or any account that is not
served on that node.

## Adding one

Nodes → Add node. You supply an address, an SSH user, and a password or key.
The panel then:

1. Connects once over SSH.
2. Reads the architecture and distribution.
3. Uploads its own binary — the same one the panel is running, so both ends
   agree on the wire format.
4. Writes `/etc/simorgh-node.json` with the certificate material, mode `0600`.
5. Installs and starts a systemd unit for the node process.
6. Verifies the mTLS connection.
7. **Discards the SSH credential.** It is never written to the database.

From then on the panel talks to the node over mTLS only. There is no password
or token anywhere in that path.

## Prerequisites install themselves

After a node is added, use Provision to install what its protocols need. This is
the same code the panel runs on its own host, and it does more than install
packages: it checks kernel modules, and where the running kernel lacks them it
installs a kernel that has them and **repoints the bootloader at it**.

That last part is a real change to the machine. The previous kernel stays
installed as a fallback, so a machine that fails to boot the new one is still
recoverable, but it is not a change to make on a server you cannot reboot.
Progress streams live, and a step that needs a reboot says so rather than
rebooting for you.

## What happens when a node goes down

A node is marked offline after three consecutive failed contacts — not one,
because a slow tick on a link to another country is not an outage.

Its inbounds are flagged in the UI. **The panel does not move their clients
anywhere.** That is deliberate: silently relocating users onto a node you did
not choose is worse than a visible outage on one you did. If you want them
moved, move them.

No traffic is lost. Counters are only reset when a collection succeeds, so a
node that was unreachable for an hour reports that hour's traffic on the next
successful tick.

## Quota across nodes

One account's quota is shared across every node it is served from. An account
with 10 GB placed on three nodes gets 10 GB in total, not 10 on each.

This is why accounting stays on the panel rather than on the nodes: the database
is the only place that knows an account's total. The cost is up to one tick of
overshoot per node after a quota is crossed, which is the same bound the
single-host design already had.

## Security, and its limits

- The node API accepts **only** client certificates signed by the panel's own
  CA. There is no password auth and no bearer token. This is not hardening on
  top of something else — it is the whole authentication, because a node applies
  configuration as root.
- The panel verifies the node too, so a hijacked address cannot be handed your
  inbound configuration.
- A desired state whose generation does not advance is refused, so a captured
  payload cannot be replayed to restore a client set you have since revoked.
- SSH credentials exist in memory for one call.

What is **not** protected:

- **Node keys are not encrypted at rest.** The panel has no at-rest encryption
  for any secret it stores, so encrypting only this one would be a false
  assurance — anyone holding the database file already holds every other
  credential in it. Protect the database file.
- A compromised node exposes the accounts served *on that node*. It does not
  expose the ledger, other nodes' accounts, or the CA key, which never leaves
  the panel.

## Testing status

The project's convention is to say plainly what was and was not verified.

**Verified by running it:**

- Desired-state hashing, including that reordering or map iteration cannot
  change a hash, and that every field change does.
- The certificate authority, against a real TLS listener: a foreign CA is
  refused, a client with no certificate is refused, and the panel refuses a node
  whose certificate it does not trust.
- The conformance suite, run against both the in-process runner and a client
  talking to a node server over real mTLS.
- Generation replay refusal.
- **Cross-node quota**: one account on three nodes, each reporting 4 GB against
  a 10 GB quota, ends disabled at 12 GB.
- Relay traffic de-duplication, which prevents MTProto and SSH transfers being
  billed twice.
- Every node route refusing a non-super admin, across all six routes and three
  caller kinds.
- Bootstrap's sequencing, file contents, permissions, unit and failure
  reporting — against a fake SSH connection.

**Not verified:**

- The real SSH path against a real machine: dialling, uploading and systemd on
  an actual host.
- Provisioning a remote node end to end, including the kernel-install path.
- Anything requiring a browser.

Build and smoke-test on your own servers before relying on this, the same way
you would for any code you had not personally run.
