# Simorgh Multi-Node Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let one Simorgh panel run every one of its 18 protocols on remote nodes, with prerequisites installed automatically, while quota and the reseller ledger stay centrally enforced.

**Architecture:** The panel binary gains a `node` mode serving an mTLS API. The master's own host becomes a `Node` row driven through the same `node.Runner` interface as remote nodes, so nothing above that interface is written twice. The master pushes declarative desired state hashed canonically; nodes apply it idempotently, collect traffic locally, and enforce a disabled-set the master computes from the central database.

**Tech Stack:** Go 1.26.2, GORM + SQLite, gin, gorilla/websocket, `golang.org/x/crypto/ssh`, `crypto/tls` + `crypto/x509`. All already direct dependencies — **this plan adds no new Go module.**

**Spec:** `docs/superpowers/specs/2026-08-04-multi-node-design.md`

## Global Constraints

- **Go 1.26.2 minimum** (`panel/go.mod` line 3). Do not lower it.
- **No new Go module dependencies.** Every library named above is already in `panel/go.mod`.
- **`panel/` is GPLv3**, separate from the MIT `core/`, `nodepanel/`, `protocols/`, `install.sh`. Keep new files under `panel/` GPLv3. Do not copy `panel/` code into the MIT trees or vice versa.
- **Go is NOT installed in the authoring environment.** Every `go build` / `go test` step must run on a Linux host with Go ≥1.26.2. Never mark a step passed without real command output. If a step cannot be run, say so explicitly in the commit message and the docs — this project documents unverified work rather than assuming it (see the "Testing status" sections in `README.md` and `panel/SIMORGH-FORK-NOTICE.md`).
- **Test commands run from `panel/`**, e.g. `cd panel && go test ./node/... -run TestX -v`.
- **Comment style:** this codebase explains *why*, at length, at the decision point (read `panel/web/service/procmgr.go` or the `Inbound` struct in `panel/database/model/model.go` before writing any). Match that density. Do not write comments that restate the code.
- **Zero-value-usable services.** Services here are value types whose methods are stateless (`CoreService`'s doc comment states this explicitly). New services follow the same rule.
- **Never log or return private keys, SSH passwords, or node keys.** Follow the existing masking pattern (`maskVpnOutSecrets` in `panel/web/service/vpnoutbound.go`).
- **Existing behaviour must not change for an operator who never adds a node.** Every task preserves this; Task 1 and Task 3 test it directly.

## File Structure

New package `panel/node/` — pure types, hashing, CA, and transport. Imports `database/model` and `xray` only. **Must not import `web/service`**, which is what keeps the dependency graph acyclic: `node` declares the `Runner` interface, `web/service` implements it, `main.go` wires them together.

| File | Responsibility |
|---|---|
| `panel/node/types.go` | `DesiredState`, `NodeInbound`, `CollectResult`, `ApplyResult`, `DisabledSet`, `NodeStatus` |
| `panel/node/hash.go` | Canonical encoding + `HashState` |
| `panel/node/runner.go` | The `Runner` interface |
| `panel/node/ca.go` | CA generation, node cert issuance, TLS config builders |
| `panel/node/client.go` | mTLS HTTP client used by `RemoteRunner` |
| `panel/node/server.go` | Node-mode HTTPS server; takes a `Runner` |
| `panel/node/bootstrap.go` | One-shot SSH provisioning of a new node |
| `panel/database/model/node.go` | `Node`, `InboundNode` GORM models |
| `panel/web/service/node_local.go` | `LocalRunner` — adapter over existing services |
| `panel/web/service/node_remote.go` | `RemoteRunner` — `node.Client` behind the `Runner` interface |
| `panel/web/service/node.go` | `NodeService` — DB orchestration, tick loop, offline detection |
| `panel/web/controller/node.go` | HTTP routes for node management |
| `panel/web/html/nodes.html` | Node management UI |
| `docs/NODES.md`, `docs/NODES.fa.md` | Operator docs, English + Persian |

Modified: `panel/database/db.go` (register models + seeder), `panel/main.go` (`node` subcommand), `panel/web/job/xray_traffic_job.go` (route the tick through nodes), `panel/web/web.go` (routes), `README.md` (+ new `README.fa.md`).

---

## Task 1: Node and InboundNode models with a no-op migration

**Files:**
- Create: `panel/database/model/node.go`
- Create: `panel/database/model/node_test.go`
- Modify: `panel/database/db.go` (model list at lines 32-46; seeder at `runSeeders`, line 252)

**Interfaces:**
- Consumes: nothing.
- Produces: `model.Node`, `model.InboundNode`, `model.LocalNodeName` constant.

- [ ] **Step 1: Write the failing test**

Create `panel/database/model/node_test.go`:

```go
package model

import "testing"

// A Node created for the master's own host must be usable with zero remote
// fields set: the local node has no address to dial and no certificate.
func TestLocalNodeNeedsNoRemoteFields(t *testing.T) {
	n := Node{Name: LocalNodeName, IsLocal: true, Enable: true}
	if n.Address != "" || n.APIPort != 0 || n.ServerCert != "" {
		t.Fatalf("local node should carry no remote fields, got %+v", n)
	}
	if !n.IsLocal {
		t.Fatal("IsLocal must survive construction")
	}
}

// InboundNode.Port is an override, not a duplicate: 0 means "inherit the
// inbound's own port". Encoding that as a method keeps every caller from
// re-deriving it and getting it subtly wrong.
func TestInboundNodeEffectivePort(t *testing.T) {
	if got := (InboundNode{Port: 0}).EffectivePort(443); got != 443 {
		t.Fatalf("0 should inherit the inbound port, got %d", got)
	}
	if got := (InboundNode{Port: 8443}).EffectivePort(443); got != 8443 {
		t.Fatalf("a set port should override, got %d", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd panel && go test ./database/model/ -run 'TestLocalNode|TestInboundNodeEffectivePort' -v`
Expected: FAIL — `undefined: LocalNodeName`, `undefined: Node`, `undefined: InboundNode`.

- [ ] **Step 3: Write the models**

Create `panel/database/model/node.go`:

```go
package model

// LocalNodeName is the reserved Name of the row representing the panel's own
// host. The local host is modelled as a Node rather than as the absence of one
// so that every code path above the Runner interface is written once: "apply
// this state to a node" means the same thing whether the node is this machine
// or a server in another country. Without this row, every node-aware feature
// would need a local branch and a remote branch, and the two would drift.
const LocalNodeName = "local"

// Node is one machine the panel drives. Exactly one row has IsLocal=true.
type Node struct {
	Id      int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Name    string `json:"name" form:"name" gorm:"unique"`
	Address string `json:"address" form:"address"` // public host/IP: dialled for the API, and used as the link address in subscriptions
	APIPort int    `json:"apiPort" form:"apiPort"`
	IsLocal bool   `json:"isLocal" gorm:"default:0"`
	Enable  bool   `json:"enable" form:"enable" gorm:"default:1"`

	// TLS material. ServerKey is never returned by any API: NodeService masks it
	// on the way out, the same way maskVpnOutSecrets handles outbound credentials.
	// It is NOT encrypted at rest, and that is a deliberate, documented limit
	// rather than an oversight -- the panel has no at-rest encryption facility for
	// any secret, so encrypting only this column would be a false assurance while
	// every other credential in the same file stays plain. See section 3.8 of the
	// design doc.
	ServerCert string `json:"-"`
	ServerKey  string `json:"-"`

	// Runtime state, written by NodeService rather than set by the operator.
	Status     string `json:"status" gorm:"default:offline"` // online | offline | provisioning | error
	LastSeen   int64  `json:"lastSeen" gorm:"default:0"`
	LastError  string `json:"lastError"`
	Version    string `json:"version"`
	Arch       string `json:"arch"`
	Distro     string `json:"distro"`
	StateHash  string `json:"stateHash"`            // hash the node last confirmed applying
	Generation int64  `json:"generation" gorm:"default:0"`
	// FailedTicks counts consecutive failed contacts. Three in a row flips Status
	// to offline. A counter rather than a timestamp because it is the retry policy
	// that matters, not wall-clock: a slow tick should not mark a healthy node down.
	FailedTicks int `json:"-" gorm:"default:0"`
}

// InboundNode places one inbound on one node.
//
// A join table rather than an Inbound.NodeId column, because the requirement is
// one inbound serving several locations at once -- a WireGuard inbound on three
// nodes is three placements, and therefore three links in one subscription. A
// column could only ever express one location per inbound, which would have to
// be torn out again to build multi-location subscriptions.
type InboundNode struct {
	Id        int `json:"id" gorm:"primaryKey;autoIncrement"`
	InboundId int `json:"inboundId" gorm:"uniqueIndex:idx_inbound_node,priority:1"`
	NodeId    int `json:"nodeId" gorm:"uniqueIndex:idx_inbound_node,priority:2"`

	// Port 0 inherits Inbound.Port. Per-placement because two nodes may not have
	// the same port free, and forcing one port across every node would make a
	// single busy port on one node block the placement everywhere.
	Port   int    `json:"port" gorm:"default:0"`
	Listen string `json:"listen"`
	Enable bool   `json:"enable" gorm:"default:1"`

	// LastError is this placement's own failure, kept per placement so the UI can
	// say "up on Frankfurt, failed on Helsinki" instead of marking the whole
	// inbound broken because one node refused it.
	LastError string `json:"lastError"`
}

// EffectivePort resolves the placement's port against the inbound's own.
func (n InboundNode) EffectivePort(inboundPort int) int {
	if n.Port == 0 {
		return inboundPort
	}
	return n.Port
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd panel && go test ./database/model/ -run 'TestLocalNode|TestInboundNodeEffectivePort' -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Register the models and write the seeder**

In `panel/database/db.go`, add to the `models` slice in `initModels()` (after `&model.CustomGeoResource{},`):

```go
		&model.Node{},
		&model.InboundNode{},
```

Then add this function to `db.go` and call it from `runSeeders` (guard it with the existing `HistoryOfSeeders` mechanism, seeder name `"seed_local_node"`):

```go
// seedLocalNode creates the row representing this host and places every
// pre-existing inbound on it.
//
// This is what makes multi-node a no-op upgrade: an operator who never adds a
// node ends up with one local node holding exactly the inbounds they already
// had, and every code path behaves as it did before. Idempotent, so re-running
// it cannot duplicate placements.
func seedLocalNode() error {
	var local model.Node
	err := db.Where("is_local = ?", true).First(&local).Error
	if err == gorm.ErrRecordNotFound {
		local = model.Node{Name: model.LocalNodeName, IsLocal: true, Enable: true, Status: "online"}
		if err := db.Create(&local).Error; err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	var inbounds []model.Inbound
	if err := db.Select("id").Find(&inbounds).Error; err != nil {
		return err
	}
	for _, in := range inbounds {
		var count int64
		if err := db.Model(&model.InboundNode{}).
			Where("inbound_id = ?", in.Id).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			continue // already placed somewhere; never second-guess the operator
		}
		if err := db.Create(&model.InboundNode{
			InboundId: in.Id, NodeId: local.Id, Enable: true,
		}).Error; err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 6: Write the seeder test**

Add to `panel/database/db_node_seed_test.go` (new file, package `database`) a test that: opens an in-memory SQLite DB via the package's existing test helper pattern (mirror `panel/database/db_wal_test.go`), creates two inbounds, runs `seedLocalNode()` twice, then asserts exactly one local node exists and exactly two `InboundNode` rows exist — proving idempotency.

- [ ] **Step 7: Run the full database and model suites**

Run: `cd panel && go test ./database/... -v`
Expected: PASS, including the pre-existing `db_wal_test.go` and `model_test.go`.

- [ ] **Step 8: Commit**

```bash
git add panel/database/model/node.go panel/database/model/node_test.go panel/database/db.go panel/database/db_node_seed_test.go
git commit -m "feat(node): model nodes and inbound placements

The local host gets a Node row so every path above the Runner interface is
written once instead of branching local-vs-remote. Placements are a join
table rather than an Inbound.NodeId column because one inbound must be able
to serve several locations at once.

Upgrade is a no-op: the seeder places existing inbounds on the local node."
```

---

## Task 2: Canonical desired state and hashing

**Files:**
- Create: `panel/node/types.go`, `panel/node/hash.go`, `panel/node/hash_test.go`

**Interfaces:**
- Consumes: `model.Node`, `model.InboundNode` (Task 1).
- Produces: `node.DesiredState`, `node.NodeInbound`, `node.CollectResult`, `node.ApplyResult`, `node.DisabledSet`, `node.NodeStatus`, `node.HashState(DesiredState) string`.

- [ ] **Step 1: Write the failing test**

Create `panel/node/hash_test.go`:

```go
package node

import "testing"

func state() DesiredState {
	return DesiredState{
		Generation: 7,
		Inbounds: []NodeInbound{
			{InboundId: 2, Tag: "b", Protocol: "wg-c", Port: 51820, Settings: `{"x":1}`},
			{InboundId: 1, Tag: "a", Protocol: "openvpn", Port: 1194, Settings: `{"y":2}`},
		},
		Certs:    map[string][]byte{"ca.crt": []byte("CA"), "a.key": []byte("K")},
		Settings: NodeSettings{XrayAPIPort: 62789},
	}
}

// The hash must not depend on the order the master happened to build the slice
// in, or on Go's randomised map iteration. Without this, an identical state
// hashes differently between two ticks and the node restarts its daemons for
// no reason -- dropping every live connection on that node.
func TestHashIsOrderIndependent(t *testing.T) {
	a := state()
	b := state()
	b.Inbounds[0], b.Inbounds[1] = b.Inbounds[1], b.Inbounds[0]
	if HashState(a) != HashState(b) {
		t.Fatalf("reordering inbounds changed the hash: %s vs %s", HashState(a), HashState(b))
	}
}

// Generation is metadata about delivery, not about content. Two states that
// differ only in generation describe the same configuration, so they must hash
// the same -- otherwise every tick would look like a change.
func TestHashIgnoresGeneration(t *testing.T) {
	a := state()
	b := state()
	b.Generation = 99
	if HashState(a) != HashState(b) {
		t.Fatal("generation must not affect the hash")
	}
}

// The other half of the contract: any real change must be visible.
func TestHashDetectsEveryFieldChange(t *testing.T) {
	base := HashState(state())
	for name, mutate := range map[string]func(*DesiredState){
		"port":     func(s *DesiredState) { s.Inbounds[0].Port = 9999 },
		"settings": func(s *DesiredState) { s.Inbounds[0].Settings = `{"x":2}` },
		"tag":      func(s *DesiredState) { s.Inbounds[0].Tag = "zzz" },
		"protocol": func(s *DesiredState) { s.Inbounds[0].Protocol = "l2tp" },
		"cert":     func(s *DesiredState) { s.Certs["ca.crt"] = []byte("OTHER") },
		"newcert":  func(s *DesiredState) { s.Certs["extra.crt"] = []byte("E") },
		"drop":     func(s *DesiredState) { s.Inbounds = s.Inbounds[:1] },
		"nodeset":  func(s *DesiredState) { s.Settings.XrayAPIPort = 1 },
	} {
		s := state()
		mutate(&s)
		if HashState(s) == base {
			t.Errorf("changing %s did not change the hash", name)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd panel && go test ./node/ -run TestHash -v`
Expected: FAIL — `undefined: DesiredState`, `undefined: HashState`.

- [ ] **Step 3: Write the types**

Create `panel/node/types.go`:

```go
// Package node carries the types, hashing, certificate handling and transport a
// panel uses to drive another machine.
//
// It deliberately does NOT import web/service. The Runner interface is declared
// here and implemented there, and main.go wires the two together; the
// alternative -- this package calling into the services directly -- is an import
// cycle, because the services also need to drive nodes.
package node

import "github.com/mhsanaei/3x-ui/v2/xray"

// NodeInbound is one inbound as a node needs to see it: everything required to
// raise the daemon, and nothing about who owns it or what they have paid. The
// node has no database and no notion of accounts beyond the credentials its
// protocols must authenticate.
type NodeInbound struct {
	InboundId      int    `json:"inboundId"`
	Tag            string `json:"tag"`
	Protocol       string `json:"protocol"`
	Listen         string `json:"listen"`
	Port           int    `json:"port"`
	Settings       string `json:"settings"`
	StreamSettings string `json:"streamSettings"`
	Sniffing       string `json:"sniffing"`
	Enable         bool   `json:"enable"`

	// Speed/limit policy the node enforces locally, mirroring the Inbound columns
	// of the same name. Carried per inbound rather than looked up, because the
	// node cannot query the panel database.
	SpeedLimitEnable   bool  `json:"speedLimitEnable"`
	SpeedLimitSeparate bool  `json:"speedLimitSeparate"`
	SpeedLimitDown     int   `json:"speedLimitDown"`
	SpeedLimitUp       int   `json:"speedLimitUp"`
	SpeedLimitAfter    int64 `json:"speedLimitAfter"`
	IPLimit            int   `json:"ipLimit"`
	IPLimitStrategy    string `json:"ipLimitStrategy"`
}

// NodeSettings is the subset of panel settings a node honours.
type NodeSettings struct {
	XrayAPIPort int `json:"xrayApiPort"`
}

// DesiredState is the whole of what a node should be running. The master sends
// state, never commands: a node that reboots, loses its disk, or misses an hour
// of updates converges on the next push, and drift is detectable by comparing
// hashes. Commands would require the master to track what the node currently
// has, with no way to notice when that belief went wrong.
type DesiredState struct {
	Generation int64             `json:"generation"`
	Hash       string            `json:"hash"`
	Inbounds   []NodeInbound     `json:"inbounds"`
	Certs      map[string][]byte `json:"certs"`
	Settings   NodeSettings      `json:"settings"`
}

// ApplyResult reports per-inbound outcomes. Partial failure is normal and is
// reported as such: one inbound whose port is taken must not mark the other
// seventeen failed.
type ApplyResult struct {
	Hash    string         `json:"hash"`
	Applied []int          `json:"applied"` // inbound ids now running
	Errors  map[int]string `json:"errors"`  // inbound id -> failure
}

// CollectResult is one node's traffic tick, in exactly the types
// InboundService.AddTraffic already consumes, so central accounting needs no
// translation layer and no second code path.
type CollectResult struct {
	Traffics       []*xray.Traffic       `json:"traffics"`
	ClientTraffics []*xray.ClientTraffic `json:"clientTraffics"`
	OnlineEmails   []string              `json:"onlineEmails"`
}

// DisabledSet is what the master sends back after accounting: the accounts a
// node must stop serving. Computed centrally because the database is the only
// place that knows an account's total usage across every node -- enforcing
// per-node would let a 10GB account spend 10GB on each of three nodes.
type DisabledSet struct {
	Emails []string `json:"emails"`
}

// NodeStatus is what a node reports about itself.
type NodeStatus struct {
	Version   string       `json:"version"`
	Arch      string       `json:"arch"`
	Distro    string       `json:"distro"`
	StateHash string       `json:"stateHash"`
	Cores     []CoreStatus `json:"cores"`
	System    SystemStatus `json:"system"`
}

// CoreStatus and SystemStatus mirror the service-layer types of the same names.
// They are re-declared here rather than imported because this package must not
// import web/service; the node server converts between them at the boundary.
type CoreStatus struct {
	Name     string         `json:"name"`
	State    string         `json:"state"`
	Detail   string         `json:"detail"`
	Version  string         `json:"version"`
	Inbounds int            `json:"inbounds"`
	Extra    map[string]any `json:"extra,omitempty"`
}

type ModuleStatus struct {
	Name     string `json:"name"`
	Loaded   bool   `json:"loaded"`
	Optional bool   `json:"optional,omitempty"`
}

type SystemStatus struct {
	IpForward bool           `json:"ipForward"`
	Nftables  bool           `json:"nftables"`
	Iproute   bool           `json:"iproute"`
	Modules   []ModuleStatus `json:"modules"`
	ModulesOK bool           `json:"modulesOk"`
}

// ProvisionStep mirrors service.ProvisionStep for the same reason.
type ProvisionStep struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Warn bool   `json:"warn,omitempty"`
	Msg  string `json:"msg"`
	Log  string `json:"log,omitempty"`
}
```

- [ ] **Step 4: Write the hashing**

Create `panel/node/hash.go`:

```go
package node

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"strconv"
)

// HashState fingerprints the CONTENT of a desired state.
//
// Hand-rolled rather than json.Marshal + sha256, because Go's map marshalling
// sorts keys but its struct field order is tied to declaration order: adding a
// field in the middle of NodeInbound would silently change every node's hash and
// restart every daemon on every node at once. Writing the fields explicitly
// makes that impossible to do by accident -- a new field only affects the hash
// when someone adds it here on purpose.
//
// Generation and Hash are excluded: they describe the delivery, not the
// configuration. Including Generation would make every tick look like a change.
func HashState(s DesiredState) string {
	h := sha256.New()

	ins := make([]NodeInbound, len(s.Inbounds))
	copy(ins, s.Inbounds)
	// Sort by id so the master's slice order cannot affect the result. Order
	// carries no meaning here: a node runs all of them.
	sort.Slice(ins, func(i, j int) bool { return ins[i].InboundId < ins[j].InboundId })

	for _, in := range ins {
		writeField(h, strconv.Itoa(in.InboundId))
		writeField(h, in.Tag)
		writeField(h, in.Protocol)
		writeField(h, in.Listen)
		writeField(h, strconv.Itoa(in.Port))
		writeField(h, in.Settings)
		writeField(h, in.StreamSettings)
		writeField(h, in.Sniffing)
		writeField(h, strconv.FormatBool(in.Enable))
		writeField(h, strconv.FormatBool(in.SpeedLimitEnable))
		writeField(h, strconv.FormatBool(in.SpeedLimitSeparate))
		writeField(h, strconv.Itoa(in.SpeedLimitDown))
		writeField(h, strconv.Itoa(in.SpeedLimitUp))
		writeField(h, strconv.FormatInt(in.SpeedLimitAfter, 10))
		writeField(h, strconv.Itoa(in.IPLimit))
		writeField(h, in.IPLimitStrategy)
	}

	names := make([]string, 0, len(s.Certs))
	for name := range s.Certs {
		names = append(names, name)
	}
	sort.Strings(names) // Go randomises map iteration; without this the hash is nondeterministic
	for _, name := range names {
		writeField(h, name)
		writeField(h, string(s.Certs[name]))
	}

	writeField(h, strconv.Itoa(s.Settings.XrayAPIPort))

	return hex.EncodeToString(h.Sum(nil))
}

// writeField length-prefixes each value so that concatenation is unambiguous:
// without it, {"ab","c"} and {"a","bc"} hash identically, and two different
// configurations would be indistinguishable.
func writeField(h io.Writer, v string) {
	_, _ = io.WriteString(h, strconv.Itoa(len(v)))
	_, _ = io.WriteString(h, ":")
	_, _ = io.WriteString(h, v)
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd panel && go test ./node/ -run TestHash -v`
Expected: PASS (3 tests).

- [ ] **Step 6: Add the length-prefix regression test**

Append to `panel/node/hash_test.go`:

```go
// Guards the length prefix in writeField. Without it these two states -- which
// are genuinely different configurations -- collide.
func TestHashDistinguishesFieldBoundaries(t *testing.T) {
	a := DesiredState{Inbounds: []NodeInbound{{InboundId: 1, Tag: "ab", Protocol: "c"}}}
	b := DesiredState{Inbounds: []NodeInbound{{InboundId: 1, Tag: "a", Protocol: "bc"}}}
	if HashState(a) == HashState(b) {
		t.Fatal("field boundaries are not encoded; concatenation is ambiguous")
	}
}
```

Run: `cd panel && go test ./node/ -v`
Expected: PASS (4 tests).

- [ ] **Step 7: Commit**

```bash
git add panel/node/types.go panel/node/hash.go panel/node/hash_test.go
git commit -m "feat(node): desired-state types and canonical hashing

Hashing is hand-rolled rather than json.Marshal so that adding a struct field
cannot silently change every node's hash and restart every daemon at once.
Fields are length-prefixed so {ab,c} and {a,bc} cannot collide, and map keys
are sorted because Go randomises map iteration."
```

---

## Task 3: The Runner interface and LocalRunner

This is the task that validates the whole design: the master starts driving its **own** host through the node abstraction, with no network code in sight. If the existing suite still passes afterwards, the abstraction is behaviour-preserving.

**Files:**
- Create: `panel/node/runner.go`
- Create: `panel/web/service/node_local.go`, `panel/web/service/node_local_test.go`

**Interfaces:**
- Consumes: `node.DesiredState`, `node.CollectResult`, `node.DisabledSet`, `node.NodeStatus`, `node.ProvisionStep` (Task 2).
- Produces: `node.Runner` interface; `service.LocalRunner` implementing it; `service.NewLocalRunner() *LocalRunner`.

- [ ] **Step 1: Write the interface**

Create `panel/node/runner.go`:

```go
package node

import "context"

// Runner is how the master drives one machine.
//
// Two implementations exist and must stay interchangeable: LocalRunner calls the
// panel's existing services in-process, RemoteRunner marshals the same calls
// over mTLS. Every feature above this line is therefore written once. The
// conformance suite in web/service/node_conformance_test.go runs the same
// assertions against both, which is what stops them drifting apart.
type Runner interface {
	// Apply makes the machine match state, idempotently. Returning without error
	// means every inbound in ApplyResult.Applied is running and every one in
	// ApplyResult.Errors is not; a partial failure is a normal result, not an
	// error return.
	Apply(ctx context.Context, state DesiredState) (ApplyResult, error)

	// Collect reads and RESETS this machine's traffic counters. Reset-on-read is
	// what makes a missed tick lossless in one direction only: the caller must
	// persist what it receives, because a second Collect will not return it
	// again.
	Collect(ctx context.Context) (CollectResult, error)

	// Enforce stops serving the named accounts. Level-triggered: the caller sends
	// the whole disabled set every tick, so a missed message self-corrects rather
	// than leaving an over-quota account running until it reconnects.
	Enforce(ctx context.Context, disabled DisabledSet) error

	// Provision installs prerequisites for the named cores, streaming progress.
	// The channel closes when provisioning finishes.
	Provision(ctx context.Context, cores []string) (<-chan ProvisionStep, error)

	// Status reports what the machine is running.
	Status(ctx context.Context) (NodeStatus, error)

	// Logs returns recent output for one core.
	Logs(ctx context.Context, core string) (string, error)
}
```

- [ ] **Step 2: Write the failing test**

Create `panel/web/service/node_local_test.go`:

```go
package service

import (
	"context"
	"testing"

	"github.com/mhsanaei/3x-ui/v2/node"
)

// Compile-time proof that LocalRunner satisfies the interface. This assertion
// failing at build time is far cheaper than discovering the gap at runtime on a
// production node.
var _ node.Runner = (*LocalRunner)(nil)

// Status must work on a bare host with nothing configured: this is the very
// first call a freshly bootstrapped node answers, and returning an error there
// would make a healthy new node look broken.
func TestLocalRunnerStatusOnEmptyHost(t *testing.T) {
	st, err := NewLocalRunner().Status(context.Background())
	if err != nil {
		t.Fatalf("Status on an unconfigured host must not error: %v", err)
	}
	if st.Version == "" {
		t.Error("Status must always report a version")
	}
	if st.Arch == "" {
		t.Error("Status must always report an architecture")
	}
}

// Enforce with nobody to disable is the overwhelmingly common tick. It must be
// a cheap no-op rather than an error, or the logs fill with failures on a
// perfectly healthy panel.
func TestLocalRunnerEnforceEmptyIsNoOp(t *testing.T) {
	if err := NewLocalRunner().Enforce(context.Background(), node.DisabledSet{}); err != nil {
		t.Fatalf("empty enforce should be a no-op: %v", err)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `cd panel && go test ./web/service/ -run TestLocalRunner -v`
Expected: FAIL — `undefined: LocalRunner`, `undefined: NewLocalRunner`.

- [ ] **Step 4: Implement LocalRunner**

Create `panel/web/service/node_local.go`. It adapts the services that already exist — write no new protocol logic:

```go
package service

import (
	"context"
	"runtime"

	"github.com/mhsanaei/3x-ui/v2/config"
	"github.com/mhsanaei/3x-ui/v2/node"
)

// LocalRunner drives the machine the panel is running on.
//
// Every method here is an adapter: the work is done by services that predate
// nodes entirely (CoreService, XrayService, the per-protocol services). Routing
// the panel's own host through this type -- rather than special-casing "local"
// above the interface -- is what proves the abstraction is behaviour-preserving,
// because the existing test suite exercises these same paths.
type LocalRunner struct {
	coreService  CoreService
	xrayService  XrayService
	inboundSvc   InboundService
}

func NewLocalRunner() *LocalRunner { return &LocalRunner{} }

func (r *LocalRunner) Status(ctx context.Context) (node.NodeStatus, error) {
	cores := r.coreService.GetCoresStatus()
	out := make([]node.CoreStatus, 0, len(cores))
	for _, c := range cores {
		out = append(out, node.CoreStatus{
			Name: c.Name, State: string(c.State), Detail: c.Detail,
			Version: c.Version, Inbounds: c.Inbounds, Extra: c.Extra,
		})
	}
	sys := r.coreService.GetSystemStatus()
	mods := make([]node.ModuleStatus, 0, len(sys.Modules))
	for _, m := range sys.Modules {
		mods = append(mods, node.ModuleStatus{Name: m.Name, Loaded: m.Loaded, Optional: m.Optional})
	}
	return node.NodeStatus{
		Version: config.GetVersion(),
		Arch:    runtime.GOARCH,
		Distro:  detectDistro(),
		Cores:   out,
		System: node.SystemStatus{
			IpForward: sys.IpForward, Nftables: sys.Nftables, Iproute: sys.Iproute,
			Modules: mods, ModulesOK: sys.ModulesOK,
		},
	}, nil
}

func (r *LocalRunner) Provision(ctx context.Context, cores []string) (<-chan node.ProvisionStep, error) {
	ch := make(chan node.ProvisionStep, 32)
	go func() {
		defer close(ch)
		for _, s := range r.coreService.Provision(cores) {
			select {
			case ch <- node.ProvisionStep{Name: s.Name, OK: s.OK, Warn: s.Warn, Msg: s.Msg, Log: s.Log}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func (r *LocalRunner) Logs(ctx context.Context, core string) (string, error) {
	return r.coreService.CoreLogs(core), nil
}
```

`Apply`, `Collect`, and `Enforce` are written in Task 4 and Task 8 respectively; for now add them as methods returning `errNotYetImplemented` so the interface assertion compiles, and note in a comment which task fills each in. `detectDistro()` reuses the existing distro detection in `panel/web/service/distro_support.go` — read that file and call the existing helper rather than writing a second one.

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd panel && go test ./web/service/ -run TestLocalRunner -v`
Expected: PASS (2 tests).

- [ ] **Step 6: Prove the abstraction changed nothing**

Run the whole pre-existing suite: `cd panel && go test ./... 2>&1 | tail -40`
Expected: the same pass/fail set as before this task. Record the baseline by running it on the previous commit first (`git stash && go test ./... ; git stash pop`) and compare. **Any newly failing test is a defect in this task, not a flaky test.**

- [ ] **Step 7: Commit**

```bash
git add panel/node/runner.go panel/web/service/node_local.go panel/web/service/node_local_test.go
git commit -m "feat(node): Runner interface and LocalRunner adapter

LocalRunner adds no protocol logic -- it adapts CoreService and the
per-protocol services that already exist. Routing the panel's own host
through the same interface remote nodes use is what keeps every node-aware
feature from being written twice."
```

---

## Task 4: Building desired state, and LocalRunner.Apply

**Files:**
- Create: `panel/web/service/node_state.go`, `panel/web/service/node_state_test.go`
- Modify: `panel/web/service/node_local.go` (fill in `Apply`)

**Interfaces:**
- Consumes: `model.Node`, `model.InboundNode`, `node.DesiredState`, `node.HashState`.
- Produces: `service.BuildDesiredState(nodeId int) (node.DesiredState, error)`.

- [ ] **Step 1: Write the failing test**

Create `panel/web/service/node_state_test.go` with tests asserting that `BuildDesiredState`:
1. Returns only inbounds placed on the given node.
2. Applies `InboundNode.Port` as an override and inherits `Inbound.Port` when the override is 0.
3. Excludes an inbound whose placement is disabled, **and** one whose inbound is disabled.
4. Sets `Hash` to `node.HashState` of itself with `Hash` cleared — i.e. the hash never covers itself.
5. Carries the speed-limit and IP-limit columns through unchanged.

Use the in-memory SQLite pattern from `panel/web/service/inbound_reseller_test.go`.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd panel && go test ./web/service/ -run TestBuildDesiredState -v`
Expected: FAIL — `undefined: BuildDesiredState`.

- [ ] **Step 3: Implement BuildDesiredState**

In `panel/web/service/node_state.go`, join `Inbound` with `InboundNode` on the node id, map each row to a `node.NodeInbound` copying the fields listed in Task 2's type, then set `Hash` last:

```go
	state.Hash = "" // never let a previous hash feed into the new one
	state.Hash = node.HashState(state)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd panel && go test ./web/service/ -run TestBuildDesiredState -v`
Expected: PASS (5 tests).

- [ ] **Step 5: Implement LocalRunner.Apply**

Replace the stub. `Apply` writes the state through the existing per-protocol paths and restarts only what changed:

```go
// Apply is idempotent and change-gated. The gate is not an optimisation: the
// tick runs every few seconds, and re-raising unchanged daemons would drop every
// live connection on the node on every tick.
func (r *LocalRunner) Apply(ctx context.Context, state node.DesiredState) (node.ApplyResult, error) {
	res := node.ApplyResult{Hash: state.Hash, Errors: map[int]string{}}
	if state.Hash != "" && state.Hash == r.lastHash {
		for _, in := range state.Inbounds {
			res.Applied = append(res.Applied, in.InboundId)
		}
		return res, nil
	}
	// ... write certs, persist inbound rows for this host, then restart cores ...
	r.lastHash = state.Hash
	return res, nil
}
```

Add `lastHash string` and a `sync.Mutex` to `LocalRunner`, and note in a comment that the field is per-process: a restarted panel re-applies once, which is correct because it cannot know what state the daemons are in.

- [ ] **Step 6: Test the change gate**

Add a test asserting a second `Apply` with the same hash reports every inbound applied without touching the cores (assert by calling it twice and checking the second returns quickly with no error and the same `Applied` set).

Run: `cd panel && go test ./web/service/ -run 'TestBuildDesiredState|TestLocalRunnerApply' -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add panel/web/service/node_state.go panel/web/service/node_state_test.go panel/web/service/node_local.go
git commit -m "feat(node): build desired state per node and apply it locally

Apply is gated on the state hash. The gate is correctness, not speed: the
tick runs every few seconds and re-raising unchanged daemons would drop every
live connection on the node each time."
```

---

## Task 5: Certificate authority and node identity

**Files:**
- Create: `panel/node/ca.go`, `panel/node/ca_test.go`

**Interfaces:**
- Produces: `node.GenerateCA() (certPEM, keyPEM []byte, err error)`, `node.IssueNodeCert(caCert, caKey []byte, host string) (certPEM, keyPEM []byte, err error)`, `node.MasterTLSConfig(caCert, clientCert, clientKey []byte, serverName string) (*tls.Config, error)`, `node.NodeTLSConfig(caCert, serverCert, serverKey []byte) (*tls.Config, error)`.

- [ ] **Step 1: Write the failing test**

Create `panel/node/ca_test.go`. The tests must assert, over a real in-process TLS listener:
1. A client presenting a cert signed by the CA connects successfully.
2. A client presenting a cert from a **different** CA is refused. (This is the test that matters: a node applies arbitrary configuration as root, so an unauthenticated node API is a remote root shell.)
3. A client presenting **no** certificate is refused.
4. `IssueNodeCert` sets the host in SANs, for both an IP and a DNS name.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd panel && go test ./node/ -run TestCA -v`
Expected: FAIL — `undefined: GenerateCA`.

- [ ] **Step 3: Implement ca.go**

Use `crypto/ecdsa` with P-256, `crypto/x509`, 10-year CA validity, 2-year node certs. `NodeTLSConfig` must set:

```go
	ClientAuth: tls.RequireAndVerifyClientCert,
	ClientCAs:  pool,
	MinVersion: tls.VersionTLS13,
```

`RequireAndVerifyClientCert` is the whole security boundary — write a comment saying so, because `RequireAnyClientCert` looks similar and silently accepts any self-signed certificate.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd panel && go test ./node/ -run TestCA -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add panel/node/ca.go panel/node/ca_test.go
git commit -m "feat(node): mTLS certificate authority for node identity

RequireAndVerifyClientCert is the entire security boundary: a node applies
configuration as root, so an unauthenticated node API is a remote root shell.
Tested against a real listener with a foreign CA and with no client cert."
```

---

## Task 6: Node-mode server and RemoteRunner, with a shared conformance suite

**Files:**
- Create: `panel/node/client.go`, `panel/node/server.go`
- Create: `panel/web/service/node_remote.go`
- Create: `panel/web/service/node_conformance_test.go`
- Modify: `panel/main.go` (add the `node` subcommand)

**Interfaces:**
- Consumes: `node.Runner`, the TLS builders from Task 5.
- Produces: `node.NewServer(r Runner, cfg ServerConfig) *Server`, `node.NewClient(cfg ClientConfig) *Client`, `service.NewRemoteRunner(...) *RemoteRunner`.

- [ ] **Step 1: Write the conformance suite first**

Create `panel/web/service/node_conformance_test.go`. One table-driven suite, run against **both** runners — `LocalRunner` directly, and `RemoteRunner` pointed at an in-process `node.Server` wrapping a `LocalRunner`. Assert identical results for `Status`, `Apply` (including the change gate and partial-failure shape), `Collect`, `Enforce`, and `Logs`.

This suite is the mechanism that keeps local and remote from diverging. Write it before either implementation so it cannot be shaped to fit whatever they happen to do.

- [ ] **Step 2: Run to verify it fails**

Run: `cd panel && go test ./web/service/ -run TestRunnerConformance -v`
Expected: FAIL — `undefined: NewRemoteRunner`.

- [ ] **Step 3: Implement the server**

`panel/node/server.go`: a `gin` engine over `tls.Listen` with `NodeTLSConfig`. Routes `POST /apply`, `POST /enforce`, `POST /provision`, `GET /status`, `GET /logs/:core`, `GET /ws`. Each unmarshals, calls the injected `Runner`, and marshals the result. **Reject any `DesiredState` whose `Generation` is not greater than the highest already seen** — this is what stops a captured payload from rolling a node back to a revoked client set. Test that rejection explicitly.

- [ ] **Step 4: Implement the client and RemoteRunner**

`panel/node/client.go` wraps `http.Client` with `MasterTLSConfig` and a per-call timeout. `panel/web/service/node_remote.go` implements `node.Runner` over it, with `var _ node.Runner = (*RemoteRunner)(nil)`.

- [ ] **Step 5: Add the `node` subcommand to main.go**

In `panel/main.go`, alongside the existing subcommands, add `node` which reads a config file (CA cert, server cert, server key, listen port), builds a `LocalRunner`, and serves. It must **not** open the database, start the web server, or start the cron scheduler — a node has no database and no jobs.

- [ ] **Step 6: Run the conformance suite**

Run: `cd panel && go test ./web/service/ -run TestRunnerConformance -v`
Expected: PASS, with each assertion passing for both runners.

- [ ] **Step 7: Run everything**

Run: `cd panel && go build ./... && go test ./... 2>&1 | tail -40`
Expected: build clean; same pass set as the Task 3 baseline plus the new tests.

- [ ] **Step 8: Commit**

```bash
git add panel/node/client.go panel/node/server.go panel/web/service/node_remote.go panel/web/service/node_conformance_test.go panel/main.go
git commit -m "feat(node): node-mode server and RemoteRunner

One conformance suite runs against both runners, which is what keeps the
local and remote paths from drifting apart. Desired states with a
non-increasing generation are refused so a captured payload cannot roll a
node back to a revoked client set."
```

---

## Task 7: NodeService, the tick loop, and offline detection

**Files:**
- Create: `panel/web/service/node.go`, `panel/web/service/node_test.go`

**Interfaces:**
- Produces: `service.NodeService` with `List`, `Get`, `Create`, `Delete`, `RunnerFor(nodeId int) (node.Runner, error)`, `Tick(ctx)`, `MarkResult(nodeId int, err error)`.

- [ ] **Step 1: Write the failing tests**

`panel/web/service/node_test.go` must assert:
1. `RunnerFor` returns a `*LocalRunner` for the local node and a `*RemoteRunner` for any other.
2. Two failed contacts leave a node `online`; the **third** flips it to `offline`. (Off-by-one here means a healthy node with one slow tick gets marked down.)
3. A success resets `FailedTicks` to 0 and restores `online`.
4. `List` never returns `ServerKey` — assert the marshalled JSON contains neither the key nor the string `PRIVATE KEY`.
5. Deleting the local node is refused.

- [ ] **Step 2: Run to verify they fail**

Run: `cd panel && go test ./web/service/ -run TestNodeService -v`
Expected: FAIL — `undefined: NodeService`.

- [ ] **Step 3: Implement NodeService**

Ticks run **in parallel across nodes** with a per-node timeout: one unreachable node in another country must not stall the tick for every other node. Use `errgroup`-style fan-out with `sync.WaitGroup` (no new dependency).

- [ ] **Step 4: Run tests**

Run: `cd panel && go test ./web/service/ -run TestNodeService -v`
Expected: PASS (5 tests).

- [ ] **Step 5: Commit**

```bash
git add panel/web/service/node.go panel/web/service/node_test.go
git commit -m "feat(node): NodeService with parallel ticks and offline detection

Three consecutive failures mark a node offline, not one: a single slow tick
must not take a healthy node down. Ticks fan out in parallel so one
unreachable node cannot stall every other node's tick."
```

---

## Task 8: Split the traffic tick between node and master

The highest-risk task: it changes billing. Do not start it until Task 7 is green.

**Files:**
- Modify: `panel/web/job/xray_traffic_job.go`
- Modify: `panel/web/service/node_local.go` (fill in `Collect` and `Enforce`)
- Create: `panel/web/job/node_traffic_test.go`

- [ ] **Step 1: Write the failing tests**

Assert:
1. **Single-host equivalence.** With only the local node, a tick produces byte-for-byte the same database state as the pre-node code path for the same inputs. This is the regression gate on billing.
2. **Cross-node quota.** One account placed on three nodes, each reporting 4 GB against a 10 GB quota, ends **disabled** — not at 30 GB. This is the reason accounting stays central; test it explicitly.
3. **Offline node loses nothing.** A node unreachable for two ticks, then reachable, has its accrued traffic billed on reconnect (counters reset only on a successful report).
4. **De-duplication still holds.** The `appendUnrecorded` guard for MTProto/SSH (documented at length in `xray_traffic_job.go`) must still prevent double-billing when the traffic arrives via a node.

- [ ] **Step 2: Run to verify they fail**

Run: `cd panel && go test ./web/job/ -run TestNodeTraffic -v`
Expected: FAIL.

- [ ] **Step 3: Move collection into LocalRunner.Collect**

`Collect` performs the *local* half of today's `Run()`: `GenerateAllConfigs` reconciles for wg-c/awg/gre, `ikev2Service.ReconcileDisabled`, the sweeper tick, `nftService.CollectAndResetTraffic`, the MTProto/SSH tallies with the existing `appendUnrecorded` guard, and Xray's `GetXrayTraffic`. Move this code — do not rewrite it, and keep its comments, which explain non-obvious billing decisions that took real debugging to find.

`Enforce` performs the enforcement half: the `KillDisabledSessions` calls and `DisableClients`.

- [ ] **Step 4: Rewire the job**

`XrayTrafficJob.Run` becomes: fan out `Collect` across nodes → merge → `AddTraffic` (unchanged) → compute the disabled set → fan out `Enforce`. The accounting middle stays exactly as it is.

- [ ] **Step 5: Run tests**

Run: `cd panel && go test ./web/job/ -v && go test ./web/service/ -v`
Expected: PASS, including the pre-existing `traffic_accounting_test.go` and `relay_traffic_test.go`.

- [ ] **Step 6: Commit**

```bash
git add panel/web/job/xray_traffic_job.go panel/web/job/node_traffic_test.go panel/web/service/node_local.go
git commit -m "feat(node): collect and enforce per node, account centrally

Accounting stays central because the database is the only place that knows an
account's total across every node: enforcing per-node would let a 10GB
account spend 10GB on each of three. Tested directly."
```

---

## Task 9: SSH bootstrap

**Files:**
- Create: `panel/node/bootstrap.go`, `panel/node/bootstrap_test.go`

- [ ] **Step 1: Write the failing tests**

Assert:
1. Architecture and distro detection parse real `uname -m` and `/etc/os-release` output (table-driven, on captured strings — no SSH needed).
2. **The SSH password is never retained.** After `Bootstrap` returns, assert the returned struct and the persisted node row contain neither the password nor the key. Test it by searching the marshalled JSON.
3. A failed SSH connection returns the underlying error, not a generic one.

- [ ] **Step 2–4: Implement**

`golang.org/x/crypto/ssh` (already a direct dependency). Steps: detect arch/distro → upload the binary → write the node config with CA + issued cert → install and start a systemd unit **for the node process only** (it supervises the VPN daemons itself, as `procmgr.go` does) → verify mTLS comes up → zero the credentials.

Mirror `nodepanel/`'s bootstrap, which is already tested end-to-end through the real HTTP+SSH path, rather than inventing a second mechanism.

- [ ] **Step 5: Integration test against a container**

Run against a container with sshd, as `nodepanel` does. If no container runtime is available, **say so in the commit message and in `docs/NODES.md`** rather than marking it passed.

- [ ] **Step 6: Commit**

```bash
git add panel/node/bootstrap.go panel/node/bootstrap_test.go
git commit -m "feat(node): one-shot SSH bootstrap, credentials discarded

SSH exists only to plant the binary and the certificate. The password is
never persisted, which is asserted by searching the marshalled node row."
```

---

## Task 10: Controller, routes, and UI

**Files:**
- Create: `panel/web/controller/node.go`, `panel/web/html/nodes.html`
- Modify: `panel/web/web.go`

- [ ] **Step 1: Write the failing tests**

Follow `panel/web/controller/idor_test.go` — that file exists because this codebase has already had authorization bugs, so match its rigour. Assert:
1. Every node route requires a super admin. A plain admin and a reseller both get 403.
2. No response body from any node endpoint contains `ServerKey` or `PRIVATE KEY`.
3. Deleting a node with placements returns a clear error naming the affected inbounds rather than silently orphaning them.

- [ ] **Step 2–4: Implement and pass**

Routes under the existing panel group: `GET /panel/api/nodes/list`, `POST /panel/api/nodes/add`, `POST /panel/api/nodes/del/:id`, `GET /panel/api/nodes/status/:id`, `POST /panel/api/nodes/provision/:id`, `GET /panel/api/nodes/logs/:id/:core`. Provisioning progress streams over the existing WebSocket hub (`panel/web/websocket/`), reusing the `ProvisionStep` rendering the local setup console already uses.

- [ ] **Step 5: Commit**

```bash
git add panel/web/controller/node.go panel/web/html/nodes.html panel/web/web.go
git commit -m "feat(node): node management API and UI

Every route is super-admin only and no response carries key material, both
asserted in tests modelled on the existing idor_test.go."
```

---

## Task 11: Documentation, English and Persian

**Files:**
- Create: `docs/NODES.md`, `docs/NODES.fa.md`, `README.fa.md`
- Modify: `README.md`, `panel/SIMORGH-FORK-NOTICE.md`

- [ ] **Step 1: Write `docs/NODES.md`**

Cover: what a node is, adding one, what provisioning does to the machine (it can install a kernel and repoint the bootloader — say so plainly), the security model and its limits (including that node keys are **not** encrypted at rest and why), what happens when a node goes offline, and why there is no automatic failover.

- [ ] **Step 2: Write `docs/NODES.fa.md`**

A genuine Persian translation, not a summary — same sections, same warnings, same honesty about what is untested. RTL markdown.

- [ ] **Step 3: Update `README.md` and add `README.fa.md`**

Add a nodes section. Update the **"Testing status"** section with what was and was not verified in this work — that section is the reason this project's docs are trustworthy, so it gets the same treatment: name anything that could not be run (Go was unavailable in the authoring environment; container-based bootstrap tests may not have run) instead of implying it passed.

- [ ] **Step 4: Correct `panel/SIMORGH-FORK-NOTICE.md`**

It currently states multi-node was deliberately not built and explains why. That reasoning was correct at the time and should be kept, with a note recording that the constraint has since been addressed and how.

- [ ] **Step 5: Commit**

```bash
git add docs/NODES.md docs/NODES.fa.md README.md README.fa.md panel/SIMORGH-FORK-NOTICE.md
git commit -m "docs: node documentation in English and Persian

Testing status records what could not be verified rather than implying it
passed, matching how the rest of this project's docs are written."
```

---

## Self-Review

**Spec coverage.** Every section of the design maps to a task: §3.1 node mode → Task 6 Step 5; §3.2 local-as-node → Tasks 1, 3; §3.3 declarative state + hashing → Tasks 2, 4; §3.4 mTLS transport → Tasks 5, 6; §3.5 SSH bootstrap + remote provisioning → Tasks 9, 3 (`Provision`), 10; §3.6 traffic split → Task 8; §3.7 data model → Task 1; §3.8 security → Tasks 5, 7, 10; §4 error handling → Tasks 4 (partial failure), 6 (generation replay), 7 (offline); §5 testing → the seven listed tests appear as Tasks 3 Step 6, 2, 6 Step 1, 9 Step 5, 8, 8, and 5/6; §6 deliverables → all tasks, docs in Task 11.

**Type consistency.** `node.Runner`'s six methods are declared once in Task 3 and implemented in Tasks 3, 4, 6, 8 with matching signatures. `HashState(DesiredState) string` is used identically in Tasks 2 and 4. `CollectResult` carries `[]*xray.Traffic` and `[]*xray.ClientTraffic` — the exact parameter types of `InboundService.AddTraffic`, verified against `panel/web/service/inbound.go:2675`.

**Known gaps, stated rather than hidden.** Tasks 4, 6, 7, 9, and 10 give test *specifications* rather than complete test bodies where the setup depends on this codebase's in-memory SQLite helpers, which vary by suite. The implementer must read the named reference test in each case (`inbound_reseller_test.go`, `idor_test.go`, `db_wal_test.go`) and follow its pattern. This is a real, deliberate reduction in detail for setup-heavy tests; the assertions themselves are fully specified.

---

**Plan complete and saved to `docs/superpowers/plans/2026-08-04-multi-node.md`.** Two execution options:

**1. Subagent-Driven (recommended)** — a fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** — execute tasks in this session using executing-plans, batched with checkpoints.

Which approach?
