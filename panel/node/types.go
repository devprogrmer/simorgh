// Package node carries the types, hashing, certificate handling and transport a
// panel uses to drive another machine.
//
// It deliberately does NOT import web/service. The Runner interface is declared
// here and implemented there, and main.go wires the two together. The
// alternative -- this package calling into the services directly -- is an import
// cycle, because the services also need to drive nodes.
package node

import "github.com/mhsanaei/3x-ui/v2/xray"

// NodeInbound is one inbound as a node needs to see it: everything required to
// raise the daemon, and nothing about who owns it or what they have paid.
//
// A node holds no database and has no notion of accounts beyond the credentials
// its protocols must authenticate. That is not an optimisation -- it is what
// keeps a compromised node from being a compromised customer list.
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

	// Policy the node enforces locally, mirroring the Inbound columns of the same
	// names. Carried per inbound rather than looked up, because the node cannot
	// query the panel database.
	SpeedLimitEnable   bool   `json:"speedLimitEnable"`
	SpeedLimitSeparate bool   `json:"speedLimitSeparate"`
	SpeedLimitDown     int    `json:"speedLimitDown"`
	SpeedLimitUp       int    `json:"speedLimitUp"`
	SpeedLimitAfter    int64  `json:"speedLimitAfter"`
	IPLimit            int    `json:"ipLimit"`
	IPLimitStrategy    string `json:"ipLimitStrategy"`
}

// NodeSettings is the subset of panel settings a node honours.
type NodeSettings struct {
	XrayAPIPort int `json:"xrayApiPort"`
}

// DesiredState is the whole of what a node should be running.
//
// The master sends state, never commands. A node that reboots, loses its disk,
// or misses an hour of updates converges on the next push, and drift is
// detectable by comparing hashes. Commands would instead require the master to
// track what the node currently has, with no way to notice when that belief
// went wrong -- and "the panel thinks this node is serving something it is not"
// is the failure mode that is hardest to see from the panel.
type DesiredState struct {
	// Generation is monotonic per node. The node refuses a state whose generation
	// does not advance, which is what stops a captured payload from being replayed
	// to roll a node back to a revoked client set.
	Generation int64 `json:"generation"`

	// Hash is HashState of this value with Hash itself cleared. Stored rather than
	// recomputed on arrival so the node can report back which state it applied.
	Hash string `json:"hash"`

	Inbounds []NodeInbound     `json:"inbounds"`
	Certs    map[string][]byte `json:"certs"`
	Settings NodeSettings      `json:"settings"`
}

// ApplyResult reports per-inbound outcomes.
//
// Partial failure is a normal result, not an error return: one inbound whose
// port is already taken must not mark the other seventeen failed, and the
// operator needs to see which one it was.
type ApplyResult struct {
	Hash    string         `json:"hash"`
	Applied []int          `json:"applied"` // inbound ids now running
	Errors  map[int]string `json:"errors"`  // inbound id -> why it is not
}

// CollectResult is one node's traffic tick.
//
// It carries exactly the types InboundService.AddTraffic already consumes, so
// central accounting needs no translation layer -- and therefore has no second
// place for the two to disagree about what a byte is.
type CollectResult struct {
	Traffics       []*xray.Traffic       `json:"traffics"`
	ClientTraffics []*xray.ClientTraffic `json:"clientTraffics"`
	OnlineEmails   []string              `json:"onlineEmails"`
}

// DisabledSet is what the master sends back after accounting: the accounts a
// node must stop serving.
//
// Computed centrally because the database is the only place that knows an
// account's total across every node. Enforcing per-node would let a 10 GB
// account spend 10 GB on each of three nodes.
//
// Level-triggered: the whole set goes down every tick, so a missed message
// self-corrects on the next one rather than leaving an over-quota account
// running until it happens to reconnect.
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

// CoreStatus, ModuleStatus, SystemStatus and ProvisionStep mirror the
// service-layer types of the same names. They are re-declared rather than
// imported because this package must not import web/service; the node server
// converts between them at the boundary, which is one small cost paid once
// against an import cycle that would otherwise force the Runner interface to
// live somewhere it does not belong.
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

type ProvisionStep struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Warn bool   `json:"warn,omitempty"`
	Msg  string `json:"msg"`
	Log  string `json:"log,omitempty"`
}
