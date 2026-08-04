package service

import (
	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/node"
)

// BuildDesiredState assembles everything one node should be running.
//
// The join is the whole function: an inbound reaches a node only through a
// placement, so a node is structurally incapable of being sent an inbound
// nobody put there. That matters more than it looks -- the failure it prevents
// is every node serving every inbound, which answers the same clients from
// several hosts and bills them once per host.
//
// Two independent switches must both be on. Inbound.Enable means "this inbound
// is off everywhere"; InboundNode.Enable means "this inbound is off HERE".
// Collapsing them into one would make an operator disabling a single location
// silently disable all of them.
func BuildDesiredState(nodeId int) (node.DesiredState, error) {
	db := database.GetDB()

	var placements []model.InboundNode
	if err := db.Where("node_id = ? AND enable = ?", nodeId, true).
		Find(&placements).Error; err != nil {
		return node.DesiredState{}, err
	}

	state := node.DesiredState{
		Inbounds: make([]node.NodeInbound, 0, len(placements)),
		Certs:    map[string][]byte{},
	}

	for _, p := range placements {
		var in model.Inbound
		if err := db.Where("id = ? AND enable = ?", p.InboundId, true).First(&in).Error; err != nil {
			// A disabled or deleted inbound is not an error: the placement simply
			// contributes nothing this tick. Returning an error here would let one
			// stale row stop the whole node from being updated.
			continue
		}

		state.Inbounds = append(state.Inbounds, node.NodeInbound{
			InboundId:      in.Id,
			Tag:            in.Tag,
			Protocol:       string(in.Protocol),
			Listen:         p.Listen,
			Port:           p.EffectivePort(in.Port),
			Settings:       in.Settings,
			StreamSettings: in.StreamSettings,
			Sniffing:       in.Sniffing,
			Enable:         true, // both switches were checked above

			// Policy is carried per inbound because the NODE enforces it and cannot
			// query the panel database to look it up. A column silently dropped here
			// is an account served without the cap its operator set, and nothing
			// would report that.
			SpeedLimitEnable:   in.SpeedLimitEnable,
			SpeedLimitSeparate: in.SpeedLimitSeparate,
			SpeedLimitDown:     in.SpeedLimitDown,
			SpeedLimitUp:       in.SpeedLimitUp,
			SpeedLimitAfter:    in.SpeedLimitAfter,
			IPLimit:            in.IPLimit,
			IPLimitStrategy:    in.IPLimitStrategy,
		})
	}

	// Cleared before hashing so a value left over from a previous build cannot
	// feed into the new one, which would make the hash depend on history rather
	// than on content.
	state.Hash = ""
	state.Hash = node.HashState(state)
	return state, nil
}
