# Simorgh Multi-Node — Design

**Date:** 2026-08-04
**Status:** Design approved, ready for implementation planning
**Scope:** Phase 1 of 5. This document covers the node subsystem only.

---

## 1. Why this, and why first

The user's request was to build "the best tunnel and the best panel", with a
named list of protocols (L2TP, OpenVPN, WireGuard, Xray, IPsec/IKEv2, MTProto,
Cisco/OpenConnect, PPTP, SSTP, AmneziaWG, GRE, SSH, RADIUS), an outbound system
comparable to Sanaei/Pasarguard plus L2TP/WireGuard/OpenVPN outbounds, node
management with automatic prerequisite installation, multi-location
subscriptions, a separate reseller panel, and HWID-based connection limits.

Reading the code first changed the shape of the work substantially. Most of that
list already exists in `panel/` (a fork of vpn-ui, itself on 3X-UI):

| Requested | Where it already lives |
|---|---|
| All 18 protocols | `panel/web/service/{l2tp,pptp,openvpn,openconnect,sstp,ikev2,wgc,awg,gre,mtproto,ssh,radius,xray}.go` |
| Outbound system incl. L2TP / WireGuard / OpenVPN | `panel/web/service/vpnout_*.go` (9 drivers) |
| Reseller with traffic-balance ledger | `model.ResellerProfile`, `panel/web/service/reseller.go` |
| Subscription generation | `panel/sub/` |
| **Automatic prerequisite installation** | `CoreService.Provision` — installs packages, loads kernel modules, installs a full kernel when modules are missing, and pins the bootloader to it (`core.go`, `bootloader.go`) |

So the work is not a rewrite. It is closing the five real gaps. Of those, the
node subsystem is first because two of the others (multi-location subscriptions,
and per-node reseller scoping) are defined in terms of nodes and cannot be built
before it.

**Phases 2–5, out of scope for this document,** each get their own spec:

- **Phase 2** — Multi-location subscriptions: one subscription yielding N configs
  across N nodes.
- **Phase 3** — Reseller panel on a separate URL, with mutual config invisibility
  between admin and reseller.
- **Phase 4** — HWID / device-identity connection limits, replacing IP counting.
- **Phase 5** — Core tunnel integration: using `core/` as the carrier between the
  Iranian node and foreign nodes.

---

## 2. Constraints discovered in the code

These are load-bearing. The design is shaped by them, not by preference.

**2.1 — `inbound.Listen` is used in local `net.Listen` calls.** A daemon can only
bind an address that exists on the machine it runs on. This is the whole reason
multi-node was not built in the original fork (`panel/SIMORGH-FORK-NOTICE.md`),
and it is correct. It means an inbound cannot be "moved" to a node by changing a
column; the daemon itself must run there.

**2.2 — Every non-Xray protocol bridges into Xray through a paired
`dokodemo-door` inbound** (`xray.go:320-371`). Traffic for all 18 protocols
therefore funnels through one measurement point per host. Consequence: a node
needs its own Xray, and traffic is collected per-node, not centrally.

**2.3 — Daemons are supervised as child processes, not systemd units**
(`procmgr.go`). `migrateFromSystemd` actively tears the old systemd design down.
Consequence: a node does not need systemd-unit generation; it needs the panel
binary running as a supervisor.

**2.4 — The traffic tick is split-natured** (`web/job/xray_traffic_job.go`).
Reading it closely, `Run()` does two distinguishable things:

- *Collection and enforcement*, which touch local kernel state: Xray gRPC stats,
  nftables counters, RADIUS session maps, in-process relay tallies,
  `GenerateAllConfigs` peer reconciles, `KillDisabledSessions`.
- *Accounting*, which touches the database: `AddTraffic`, quota crossing,
  reseller ledger, WebSocket broadcast.

This split is the key seam of the whole design. Collection and enforcement must
happen on the node. Accounting must stay central, because the database is the
single source of truth for quota and the reseller ledger, and splitting it would
make a client's quota enforceable only per-node — meaning a 10 GB account on 3
nodes could burn 30 GB.

**2.5 — `CoreService` is already a complete per-protocol lifecycle API**:
`GetCoresStatus`, `RestartCore`, `StopCore`, `RestartAll`, `Provision`,
`StartProvision`, `ProvisionState`, `MissingProtocols`. It is written against
the local host. Making it remotely invocable is the bulk of the node's API.

**2.6 — `SubService.resolveInboundAddress` returns the panel's own address**
when `Listen` is unset. This is the exact function Phase 2 will need to change,
and this design must leave it able to answer "which node is this link for".

---

## 3. Architecture

### 3.1 One binary, two modes

The panel binary gains a node mode. Invoked as `simorgh-panel node`, it starts an
mTLS HTTPS server exposing the node API and does not start the web UI, the
database, or the job scheduler.

Rejected alternative: a separate slim agent binary. It would have to either
duplicate ~13,000 lines of protocol driver logic — which guarantees the master
and node protocol sets drift apart, directly breaking the requirement that every
protocol work on every node — or import those packages, at which point it is this
design with an extra build target. The few megabytes of unused web assets on a
node are a smaller cost than protocol divergence.

### 3.2 The local host is a node

A `Node` row with `IsLocal = true` represents the master's own machine. Every
existing code path becomes "the local node's driver". Remote nodes run the same
code behind an RPC boundary.

This is the single most important decision in the design. Without it, every
feature added later has to be written twice — once for local, once for remote —
and the two implementations diverge. With it, `NodeRunner` is an interface with
two implementations, and everything above it is node-agnostic:

```go
// NodeRunner is how the master drives one node. LocalRunner calls the existing
// services in-process; RemoteRunner marshals the same calls over mTLS.
type NodeRunner interface {
    Apply(ctx context.Context, desired DesiredState) (ApplyResult, error)
    Collect(ctx context.Context) (CollectResult, error)
    Enforce(ctx context.Context, disabled DisabledSet) error
    Provision(ctx context.Context, cores []string) (<-chan ProvisionStep, error)
    Status(ctx context.Context) (NodeStatus, error)
    Logs(ctx context.Context, core string) (string, error)
}
```

`LocalRunner` is a thin adapter over the services that exist today. Writing it
first, and switching the master to drive its own host *through* it, is how this
design is validated before any network code exists.

### 3.3 Declarative state, not imperative commands

The master computes a node's complete desired state, hashes it, and pushes it
whenever the hash changes. The node applies it idempotently and reports what it
did.

Imperative commands ("add this inbound", "restart that daemon") were rejected:
they require the master to track what the node currently has, and any missed
message leaves the two permanently out of sync with no way to detect it. With a
declarative model, a node that reboots, loses its disk, or misses an hour of
updates converges on the next push, and drift is detectable by comparing hashes.

```go
type DesiredState struct {
    Generation int64             // monotonic, from the master
    Hash       string            // sha256 of the canonical encoding below
    Inbounds   []NodeInbound     // inbounds assigned to this node
    Certs      map[string][]byte // CA/cert/key material the protocols need
    Settings   NodeSettings      // subset of panel settings a node honours
}
```

The hash is computed over a canonical (sorted-key, stable-order) encoding so
that a semantically identical state never produces a different hash and never
triggers a spurious daemon restart.

### 3.4 Transport: master dials node, mTLS, one WebSocket

The master is always the TLS client. The node runs an HTTPS server on a
configurable port (default 62050) and accepts only client certificates signed by
the master's CA.

- **Commands** are POSTs: `/apply`, `/enforce`, `/provision`, `/status`, `/logs`.
- **Live data** rides one long-lived WebSocket the master holds open, over which
  the node pushes traffic collections, provisioning progress, and daemon log
  lines.

JSON over `gin` + `gorilla/websocket`, both already dependencies, rather than
gRPC. gRPC would add a protobuf codegen step to the build for payloads that are
small and infrequent, and would make the wire format substantially harder to
inspect when a node misbehaves — which, for a system whose failure mode is "a
server in another country is silently wrong", matters more than throughput.

**Certificate authority.** The master generates a CA on first use, stored in the
panel database. Each node gets a server certificate; the master holds one client
certificate. A node's certificate is revoked by deleting the node, and the node
refuses every connection thereafter. This is not optional hardening: a node
applies arbitrary configuration as root, so an unauthenticated node API is a
remote root shell.

### 3.5 Bootstrap: SSH once, then never again

Adding a node takes an IP, an SSH port, and a root password or key. The master
then, over a single SSH session:

1. Detects architecture and distribution.
2. Uploads the panel binary.
3. Writes the node config (master CA, node cert, node key, listen port).
4. Installs and starts a systemd unit for the node process itself. (The node
   process supervises the VPN daemons as children; systemd supervises only the
   node process.)
5. Verifies the mTLS connection comes up.
6. **Discards the SSH credentials.** They are never persisted.

This mirrors `nodepanel/`'s bootstrap, which is already tested end-to-end
through the real HTTP+SSH code path, rather than inventing a second mechanism.

Immediately after the connection is verified, the master calls `Provision` on
the node for the protocol set the operator selected, and streams its progress
into the UI using the same `ProvisionStep` rendering the local panel already
uses. This is the "prerequisites install themselves" requirement — satisfied by
routing an existing, tested code path through the new boundary, not by writing
a new installer.

### 3.6 Traffic and enforcement flow

Per tick, for each node in parallel:

```
node: collect  ──►  master: AddTraffic (DB, quota, reseller ledger)
                              │
                              ▼
                    master: compute DisabledSet
                              │
                              ▼
node: enforce  ◄──────────────┘
```

The node returns `[]*xray.Traffic` and `[]*xray.ClientTraffic` — the exact types
`AddTraffic` already consumes — so the master's accounting code is unchanged.
The master then sends back the set of accounts that are disabled, over quota, or
expired, and the node runs the `KillDisabledSessions` / `DisableClients` /
reconcile calls that already exist.

**Quota correctness across nodes.** Because accounting is central and
enforcement is a function of central state, an account on three nodes shares one
quota. The cost is up to one tick of overshoot per node after a quota is crossed
— the same bound the current single-host design already accepts and documents.

**Node offline.** Unreachable nodes are marked offline after three consecutive
failed ticks (configurable). Their inbounds are flagged in the UI. The master does
**not** reassign their clients automatically; silent reassignment would move
users onto a node the operator never chose. Traffic accrued while offline is
collected on reconnect, because the collection counters are reset only on a
successful report.

### 3.7 Data model

```go
type Node struct {
    Id           int
    Name         string
    Address      string // public host/IP, used for links and for dialling
    APIPort      int
    IsLocal      bool
    Enable       bool
    // TLS material
    ServerCert   string
    ServerKey    string // see 3.8 on how this is protected
    // Runtime, not operator-set
    Status       string // online | offline | provisioning | error
    LastSeen     int64
    Version      string
    Arch         string
    Distro       string
    StateHash    string // last hash the node confirmed applying
    Generation   int64
}

// InboundNode places one inbound on one node. The join table, rather than an
// Inbound.NodeId column, is what makes Phase 2 possible: one WireGuard inbound
// on three nodes is three placements and therefore three links in one
// subscription.
type InboundNode struct {
    Id         int
    InboundId  int `gorm:"uniqueIndex:idx_inbound_node,priority:1"`
    NodeId     int `gorm:"uniqueIndex:idx_inbound_node,priority:2"`
    Port       int // per-placement; 0 inherits Inbound.Port
    Listen     string
    Enable     bool
    LastError  string
}
```

**Migration is a no-op for existing installs.** A seeder creates the local node
row and one `InboundNode` per existing inbound pointing at it. Every current
inbound keeps its current behaviour, and an operator who never adds a node sees
no change.

### 3.8 Security boundary

- Node API: mTLS only, master CA only. No password auth, no bearer token.
- The node holds no database and no client credentials beyond what its running
  protocols require.
- SSH credentials exist only in memory during bootstrap.
- **Private key material at rest: no better than the panel's existing standard,
  and this is stated rather than glossed.** The panel has no at-rest encryption
  facility today — secrets such as VPN outbound credentials are stored plainly
  and only masked on API output (`maskVpnOutSecrets`). Node keys follow the same
  rule: masked on output, never returned by any list endpoint, protected by the
  database file's permissions. Introducing a key-management scheme for this one
  table would be a false assurance, since an attacker with the database file
  already holds every other credential in it. If at-rest encryption is wanted, it
  belongs in its own piece of work covering every secret the panel stores.
- A node applies only state signed by the generation counter it is given;
  replaying an older `DesiredState` is rejected on generation, which prevents a
  captured payload from rolling a node back to a revoked client set.

---

## 4. Error handling

| Failure | Behaviour |
|---|---|
| Bootstrap SSH fails | Node row is not created; the operator sees the actual SSH error, not a generic one |
| Node unreachable mid-operation | Marked offline after 3 failed ticks; inbounds flagged; no auto-reassignment |
| `Apply` partially fails | Node reports per-inbound errors in `ApplyResult`; the succeeded inbounds stay up; failures surface per-inbound in the UI |
| Node returns a stale hash | Master re-pushes at the next tick; persistent mismatch raises a drift alert |
| Node version mismatch after a master upgrade | Node reports its version in `Status`; the master refuses to push a state it knows the node cannot parse, and prompts to upgrade the node |
| Provision needs a reboot | Node reports it, exactly as the local path already does; the operator triggers the reboot explicitly |

The theme: a node never guesses, and the master never silently papers over a
node's failure. Every failure is attributable to a specific node and a specific
inbound.

---

## 5. Testing

The existing codebase tests heavily and honestly (`inbound_reseller_test.go`,
`traffic_accounting_test.go`, `vpnoutbound_synth_test.go`, and the namespace-based
core tests). This work holds that line.

1. **`LocalRunner` equivalence.** Before any network code exists, route the
   master's own host through `NodeRunner`. Every existing test must still pass —
   this proves the abstraction did not change behaviour.
2. **Canonical hashing.** Property test: semantically identical `DesiredState`
   values hash identically regardless of map iteration order; any semantic change
   changes the hash.
3. **Runner conformance suite.** One table-driven suite run against both
   `LocalRunner` and `RemoteRunner` (the latter over a real in-process mTLS
   listener), asserting identical results. This is what keeps the two from
   diverging.
4. **Bootstrap.** Against a container with sshd, as `nodepanel` already does.
5. **Traffic split.** A node reporting known `ClientTraffic` produces exactly the
   same database state as today's single-host path with the same input.
6. **Cross-node quota.** One account placed on three nodes, each reporting 4 GB
   against a 10 GB quota, ends disabled — not at 30 GB.
7. **Security.** A client without a master-signed certificate is refused; a
   replayed older generation is refused.

Anything that cannot be verified in this environment gets said plainly in the
docs, in the style the project already uses, rather than assumed to work.

---

## 6. Deliverables

- `NodeRunner` interface with `LocalRunner` and `RemoteRunner`.
- Node mode in the panel binary.
- `Node` and `InboundNode` models, with a no-op migration seeder.
- SSH bootstrap with credential discard.
- Remote provisioning wired to the existing `CoreService.Provision`.
- Per-node traffic collection and enforcement.
- Node management UI: add, list, status, per-core status, logs, provision
  progress, delete.
- **Documentation in English and Persian** — `docs/NODES.md` and
  `docs/NODES.fa.md`, plus node sections in the root `README.md` and a Persian
  `README.fa.md`.

---

## 7. Explicitly not in this phase

- Multi-location subscription output (Phase 2). This design makes it possible by
  modelling placements, but does not change `sub/`.
- Reseller panel separation (Phase 3).
- HWID limits (Phase 4).
- Routing node traffic through `core/`, the Simorgh tunnel (Phase 5).
- Automatic failover between nodes. Deliberate: silently moving users to a node
  the operator did not choose is worse than a visible outage on a node they did.
