package service

import (
	"fmt"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
)

// Placements are the link between an inbound and the machines that serve it.
//
// Everything else in the node subsystem was already here -- the model, the
// desired-state push, the multi-location subscription -- but with no way to
// create a placement the whole thing was unreachable from the panel. This is
// that missing turn of the loop.

// Placement is one row joined to its node, which is what a UI needs: the node id
// alone tells an operator nothing.
type Placement struct {
	Id        int    `json:"id"`
	InboundId int    `json:"inboundId"`
	NodeId    int    `json:"nodeId"`
	NodeName  string `json:"nodeName"`
	IsLocal   bool   `json:"isLocal"`
	Address   string `json:"address"`
	Port      int    `json:"port"`
	Listen    string `json:"listen"`
	Advertise string `json:"advertise"`
	Enable    bool   `json:"enable"`
	LastError string `json:"lastError"`
}

type PlacementService struct{}

// ListForInbound returns where an inbound is served from.
func (s *PlacementService) ListForInbound(inboundId int) ([]Placement, error) {
	db := database.GetDB()
	var rows []model.InboundNode
	if err := db.Where("inbound_id = ?", inboundId).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}

	out := make([]Placement, 0, len(rows))
	for _, r := range rows {
		p := Placement{
			Id: r.Id, InboundId: r.InboundId, NodeId: r.NodeId,
			Port: r.Port, Listen: r.Listen, Advertise: r.Advertise,
			Enable: r.Enable, LastError: r.LastError,
		}
		var n model.Node
		if db.First(&n, r.NodeId).Error == nil {
			p.NodeName, p.IsLocal, p.Address = n.Name, n.IsLocal, n.Address
		} else {
			// A placement pointing at a deleted node is shown rather than hidden.
			// Hiding it would leave an operator wondering why an inbound is not
			// being served anywhere they can see.
			p.NodeName = fmt.Sprintf("(missing node %d)", r.NodeId)
		}
		out = append(out, p)
	}
	return out, nil
}

// SetForInbound replaces an inbound's placements with exactly the given node ids.
//
// Declarative, matching how the rest of the node subsystem works: the caller
// sends the set it wants and this reconciles. An add/remove pair of endpoints
// would make the UI responsible for diffing, and a dropped request would leave
// the two disagreeing with nothing to detect it.
//
// Existing rows for nodes that stay are PRESERVED rather than recreated, so a
// per-placement port override survives an unrelated edit to the list.
func (s *PlacementService) SetForInbound(inboundId int, nodeIds []int) error {
	db := database.GetDB()

	var inbound model.Inbound
	if err := db.First(&inbound, inboundId).Error; err != nil {
		return fmt.Errorf("no such inbound")
	}

	// An empty set would leave the inbound served by nothing while the panel
	// still lists it as enabled -- an outage that looks like a working config.
	// Refused rather than silently accepted; disabling the inbound is how an
	// operator turns it off.
	if len(nodeIds) == 0 {
		return fmt.Errorf("an inbound must be placed on at least one node; disable the inbound instead of removing every location")
	}

	wanted := make(map[int]bool, len(nodeIds))
	for _, id := range nodeIds {
		var n model.Node
		if err := db.First(&n, id).Error; err != nil {
			return fmt.Errorf("no such node: %d", id)
		}
		wanted[id] = true
	}

	var existing []model.InboundNode
	if err := db.Where("inbound_id = ?", inboundId).Find(&existing).Error; err != nil {
		return err
	}
	have := make(map[int]bool, len(existing))
	for _, e := range existing {
		have[e.NodeId] = true
		if !wanted[e.NodeId] {
			if err := db.Delete(&model.InboundNode{}, e.Id).Error; err != nil {
				return err
			}
		}
	}

	for id := range wanted {
		if have[id] {
			continue // keep the row, and any port override on it
		}
		if err := db.Create(&model.InboundNode{
			InboundId: inboundId, NodeId: id, Enable: true,
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

// UpdatePlacement changes one placement's own settings.
//
// advertise is the address customers are given when it differs from where the
// daemon runs -- the relay topology, where a foreign node's daemon is reached
// through an Iranian server. Empty means "use the node's own address".
func (s *PlacementService) UpdatePlacement(id, port int, listen, advertise string, enable bool) error {
	if port < 0 || port > 65535 {
		return fmt.Errorf("port %d is not a port", port)
	}
	return database.GetDB().Model(&model.InboundNode{}).Where("id = ?", id).
		Updates(map[string]any{
			// A map rather than a struct, because GORM omits a struct's zero
			// values from an update: Enable=false and Port=0 (meaning "inherit")
			// would both be silently dropped, which is the same trap the model's
			// default tags had.
			"port":      port,
			"listen":    listen,
			"advertise": advertise,
			"enable":    enable,
		}).Error
}
