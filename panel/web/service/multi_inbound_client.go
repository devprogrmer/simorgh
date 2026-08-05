package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
)

// Creating one customer across several inbounds in a single action.
//
// This is how selling actually works: a customer buys "20 GB", not "20 GB of
// VLESS". They want a subscription that carries VLESS and WireGuard and
// whatever else, all drawing on the one quota. Doing that by hand meant
// creating the client once per inbound and copying the email and subscription
// id exactly right each time -- tedious, and a single typo silently produces
// two customers with two separate quotas instead of one with a shared one.
//
// The shared identity is the whole mechanism. Quota, expiry, device limit and
// the traffic ledger are all keyed on Email, so the same email on four inbounds
// IS one account with one quota. The same SubID is what makes one subscription
// link return all four configs. Neither is a convention this function invents --
// it just guarantees they match.

// MultiInboundClientRequest is one customer, and where to serve them from.
type MultiInboundClientRequest struct {
	InboundIds []int        `json:"inboundIds"`
	Client     model.Client `json:"client"`
}

// MultiInboundResult reports what happened per inbound.
//
// Partial success is a real outcome and is reported as one: an inbound whose
// protocol rejects something must not discard the accounts that were created
// correctly, and the operator needs to know which of the four to look at.
type MultiInboundResult struct {
	Created []int             `json:"created"`
	Errors  map[int]string    `json:"errors"`
	Email   string            `json:"email"`
	SubID   string            `json:"subId"`
	Skipped map[int]string    `json:"skipped"`
}

// CreateAcrossInbounds puts one customer on every inbound given.
func (s *InboundService) CreateAcrossInbounds(req MultiInboundClientRequest) (*MultiInboundResult, error) {
	if len(req.InboundIds) == 0 {
		return nil, fmt.Errorf("pick at least one inbound to create the account on")
	}
	email := strings.TrimSpace(req.Client.Email)
	if email == "" {
		return nil, fmt.Errorf("the account needs an email; it is the identity quota and expiry are keyed on")
	}
	if strings.TrimSpace(req.Client.SubID) == "" {
		return nil, fmt.Errorf("the account needs a subscription id, or its configs cannot be served as one subscription")
	}

	db := database.GetDB()
	res := &MultiInboundResult{
		Errors:  map[int]string{},
		Skipped: map[int]string{},
		Email:   email,
		SubID:   req.Client.SubID,
	}

	for _, id := range req.InboundIds {
		var inbound model.Inbound
		if err := db.First(&inbound, id).Error; err != nil {
			res.Errors[id] = "no such inbound"
			continue
		}

		// Already there is a SKIP, not an error. Re-running this to add one more
		// protocol to an existing customer is a normal thing to do, and it must
		// not report failure for the inbounds that were already correct.
		if existing, err := s.GetClients(&inbound); err == nil {
			already := false
			for _, c := range existing {
				if strings.EqualFold(strings.TrimSpace(c.Email), email) {
					already = true
					break
				}
			}
			if already {
				res.Skipped[id] = "already on this inbound"
				res.Created = append(res.Created, id)
				continue
			}
		}

		// A fresh copy per inbound. The email and subId are identical by
		// construction -- that is the point -- but anything the protocol
		// generates per account (a UUID, a key) must not be shared, or two
		// inbounds would hand out the same credential.
		c := req.Client
		c.Email = email
		c.ID = ""
		c.Password = ""

		payload, err := json.Marshal(map[string]any{"clients": []model.Client{c}})
		if err != nil {
			res.Errors[id] = err.Error()
			continue
		}
		if _, err := s.AddInboundClient(&model.Inbound{Id: id, Settings: string(payload)}); err != nil {
			res.Errors[id] = err.Error()
			continue
		}
		res.Created = append(res.Created, id)
	}

	if len(res.Created) == 0 {
		return res, fmt.Errorf("the account could not be created on any of the selected inbounds")
	}
	logger.Infof("created %q on %d inbound(s), subId %s", email, len(res.Created), res.SubID)
	return res, nil
}
