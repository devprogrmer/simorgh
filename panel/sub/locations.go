package sub

import (
	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
)

// Multi-location subscriptions: one inbound placed on several nodes yields one
// config per node, all inside the same subscription.
//
// This is what the InboundNode join table was for. An inbound on three nodes is
// three placements, so a customer holding it gets three configs -- Frankfurt,
// Helsinki, Amsterdam -- and picks between them in their client app, while all
// three bill against the one quota their account has.

// subLocation is one place an inbound is served from.
type subLocation struct {
	// Address is the node's public host. Empty means "the panel's own host",
	// which is what resolveInboundAddress already falls back to, so the local
	// node needs no special case anywhere downstream.
	Address string
	// Port is the placement's effective port. 0 means the inbound's own.
	Port int
	// Name labels the config in the customer's client app. Empty for the local
	// node, so a single-location setup produces exactly the remark it always
	// did rather than gaining a suffix nobody asked for.
	Name string
}

// linksForEveryLocation renders one config per place this inbound is served
// from.
//
// It works by pointing a COPY of the inbound at each location in turn and
// letting the existing per-protocol link generators do their job unchanged.
// Rewriting those generators to be location-aware would mean touching every one
// of the twenty protocols and giving each its own chance to get it wrong; this
// way the multi-location feature is one function and the link formats stay
// exactly as tested.
func (s *SubService) linksForEveryLocation(inbound *model.Inbound, email string) []string {
	locations := locationsFor(inbound.Id)
	links := make([]string, 0, len(locations))

	for _, loc := range locations {
		// A copy per location. Mutating the caller's inbound would leak the last
		// location's address into the traffic accounting that reads it afterwards.
		view := *inbound

		if loc.Address != "" {
			// resolveInboundAddress returns Listen when it is a real address and
			// the panel's own host otherwise, so setting it here is all that is
			// needed to point a link at a node.
			view.Listen = loc.Address
		}
		if loc.Port != 0 {
			view.Port = loc.Port
		}
		if loc.Name != "" {
			// Labelled so the customer can tell the three apart in their client
			// app. Without this they see the same remark three times and have no
			// way to choose.
			view.Remark = inbound.Remark + " · " + loc.Name
		}

		if link := s.getLink(&view, email); link != "" {
			links = append(links, link)
		}
	}
	return links
}

// locationsFor returns every place this inbound is currently served from.
//
// A single-location install and one that never adds a node both end up here with
// one placement on the local node, which returns one unnamed location -- byte for
// byte the behaviour before nodes existed. That is deliberate: the multi-location
// path is the same code, not a branch, so it cannot rot while unused.
//
// An inbound with NO placements also yields one unnamed location rather than
// none. Placements are seeded for every inbound, so an empty result means
// something is wrong with the data, and answering "serve it from the panel host"
// keeps a working config in the customer's hands while that is investigated.
// Returning nothing would silently empty their subscription.
func locationsFor(inboundId int) []subLocation {
	fallback := []subLocation{{}}

	db := database.GetDB()
	var placements []model.InboundNode
	if err := db.Where("inbound_id = ? AND enable = ?", inboundId, true).
		Find(&placements).Error; err != nil {
		return fallback
	}
	if len(placements) == 0 {
		return fallback
	}

	locations := make([]subLocation, 0, len(placements))
	for _, p := range placements {
		var n model.Node
		if db.First(&n, p.NodeId).Error != nil {
			continue
		}
		// A disabled node is left out rather than handed over as a dead endpoint.
		// A client app that cannot reach one of its configs looks broken to the
		// customer, and they cannot tell which of the three it was.
		if !n.Enable {
			continue
		}
		// An offline node still ships. Offline means "the panel has not reached it
		// for three ticks", which is very often the control channel rather than
		// the data path -- the VPN daemons keep serving whatever they last had.
		// Withholding those configs would turn a management blip into an outage
		// for customers who are connected and fine.

		loc := subLocation{Port: p.Port}
		if !n.IsLocal {
			loc.Address = n.Address
			loc.Name = n.Name
		}
		// Advertise wins over everything else, and is checked last so it does.
		//
		// It is what a client DIALS, which is not always where the daemon runs:
		// in the relay topology the daemon is on a foreign node while customers
		// connect to an Iranian server that forwards to it. Listen is not
		// consulted here at all -- that is the daemon's bind address, and a
		// daemon bound to a private interface behind a relay would otherwise
		// hand customers an address they cannot reach.
		if p.Advertise != "" {
			loc.Address = p.Advertise
		}
		locations = append(locations, loc)
	}

	if len(locations) == 0 {
		// Every placement pointed at a disabled or missing node. Same reasoning as
		// above: keep the customer working rather than hand them an empty file.
		return fallback
	}
	return locations
}
