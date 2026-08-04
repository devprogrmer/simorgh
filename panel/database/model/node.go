package model

// LocalNodeName is the reserved Name of the row representing the panel's own
// host.
//
// The local host is modelled as a Node rather than as the absence of one so that
// every code path above the Runner interface is written once: "apply this state
// to a node" means the same thing whether the node is this machine or a server
// in another country. Without this row, every node-aware feature would need a
// local branch and a remote branch, and the two would drift -- which is the
// specific failure this whole design is arranged to prevent, since a protocol
// that works locally but not on a node is worse than one that works nowhere.
const LocalNodeName = "local"

// Node status values. Strings rather than an int enum to match how Inbound
// stores IPLimitStrategy and TrafficReset: readable in a sqlite dump, and a
// value the code does not recognise degrades to "unknown" instead of silently
// meaning whatever integer it collides with.
const (
	NodeOnline       = "online"
	NodeOffline      = "offline"
	NodeProvisioning = "provisioning"
	NodeError        = "error"
)

// NodeOfflineAfterFailures is how many consecutive failed contacts mark a node
// offline.
//
// Three rather than one, because one slow tick on a link to another country is
// not an outage and flapping a node's status would flap its inbounds in the UI.
// A count rather than a wall-clock deadline: what matters is that the retry
// policy tolerated a couple of misses, not how long they took.
const NodeOfflineAfterFailures = 3

// Node is one machine the panel drives. Exactly one row has IsLocal=true.
type Node struct {
	Id   int    `json:"id" form:"id" gorm:"primaryKey;autoIncrement"`
	Name string `json:"name" form:"name" gorm:"unique"`

	// Address is the public host or IP. It is used for two different things --
	// dialling the node's API, and standing in as the link address in generated
	// subscriptions -- and they are deliberately the same field: a node reachable
	// at one address but handing clients another is a misconfiguration that should
	// be impossible to express rather than a feature.
	Address string `json:"address" form:"address"`
	APIPort int    `json:"apiPort" form:"apiPort"`

	IsLocal bool `json:"isLocal" gorm:"default:0"`
	Enable  bool `json:"enable" form:"enable" gorm:"default:1"`

	// TLS material.
	//
	// ServerKey is never returned by any API: it carries json:"-" here, and
	// NodeService masks it again on the way out, the same belt-and-braces the
	// panel applies to outbound credentials (maskVpnOutSecrets).
	//
	// It is NOT encrypted at rest, and that is a deliberate, documented limit
	// rather than an oversight. The panel has no at-rest encryption facility for
	// any secret it stores, so encrypting only this column would be a false
	// assurance: anyone holding the database file already holds every other
	// credential in it. If at-rest encryption is wanted it belongs in its own
	// piece of work covering all of them.
	ServerCert string `json:"-"`
	ServerKey  string `json:"-"`

	// Runtime state, written by NodeService rather than set by an operator.
	Status    string `json:"status" gorm:"default:offline"`
	LastSeen  int64  `json:"lastSeen" gorm:"default:0"`
	LastError string `json:"lastError"`
	Version   string `json:"version"`
	Arch      string `json:"arch"`
	Distro    string `json:"distro"`

	// StateHash is the desired-state hash the node last confirmed applying, and
	// Generation is the counter the master stamps on each push. Comparing the two
	// is how drift becomes detectable rather than something that has to be
	// noticed: a node whose hash stops matching what the master sent is visibly
	// wrong, without the master having to remember what it believes the node has.
	StateHash  string `json:"stateHash"`
	Generation int64  `json:"generation" gorm:"default:0"`

	// FailedTicks counts consecutive failed contacts; see
	// NodeOfflineAfterFailures. Not exposed: it is retry bookkeeping, and Status
	// is the answer the UI actually wants.
	FailedTicks int `json:"-" gorm:"default:0"`
}

// InboundNode places one inbound on one node.
//
// A join table rather than an Inbound.NodeId column, because the requirement is
// one inbound serving several locations at once -- a WireGuard inbound on three
// nodes is three placements, and therefore three links in one subscription. A
// column could only ever express one location per inbound and would have to be
// torn out again to build multi-location subscriptions, taking every row that
// referenced it along.
type InboundNode struct {
	Id        int `json:"id" gorm:"primaryKey;autoIncrement"`
	InboundId int `json:"inboundId" gorm:"uniqueIndex:idx_inbound_node,priority:1"`
	NodeId    int `json:"nodeId" gorm:"uniqueIndex:idx_inbound_node,priority:2"`

	// Port 0 inherits Inbound.Port -- see EffectivePort. Per placement because two
	// nodes need not have the same port free, and forcing one port across every
	// node would let a single busy port on one node block the placement
	// everywhere.
	Port   int    `json:"port" gorm:"default:0"`
	Listen string `json:"listen"`
	Enable bool   `json:"enable" gorm:"default:1"`

	// LastError is this placement's own failure, kept per placement so the UI can
	// say "up on Frankfurt, failed on Helsinki" rather than marking the whole
	// inbound broken because one node refused it.
	LastError string `json:"lastError"`
}

// EffectivePort resolves the placement's port override against the inbound's own
// port. 0 means inherit; see the Port field for why the rule lives here.
func (n InboundNode) EffectivePort(inboundPort int) int {
	if n.Port == 0 {
		return inboundPort
	}
	return n.Port
}
