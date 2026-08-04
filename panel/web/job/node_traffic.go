package job

import (
	"context"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/node"
	"github.com/mhsanaei/3x-ui/v2/web/service"
	"github.com/mhsanaei/3x-ui/v2/xray"
)

// The remote half of a traffic tick.
//
// Collection and enforcement happen ON each node, because they touch that
// machine's kernel state. Accounting stays on the master, because the database
// is the only place that knows an account's total across every node -- enforcing
// per node would let a 10GB account spend 10GB on each of three.

// nodeTickTimeout bounds one node's contribution to a tick. Generous, because a
// node on a filtered path can be slow without being broken, but bounded, because
// accounting for every other node is what waits.
const nodeTickTimeout = 20 * time.Second

// collectFromNodes gathers this tick from every enabled REMOTE node, in
// parallel.
//
// Parallel because serially one unreachable node stalls every healthy one behind
// it: five nodes at a 20s timeout turns a two-second tick into a minute and a
// half, and the traffic nobody is billing for accrues the whole time.
//
// A node that fails contributes nothing rather than failing the tick. Its
// counters are NOT reset on a failed collect, so the bytes are still there and
// arrive on the next successful one -- an offline node loses no traffic, it only
// reports it late.
func (j *XrayTrafficJob) collectFromNodes() ([]*xray.Traffic, []*xray.ClientTraffic) {
	nodes, err := remoteNodes()
	if err != nil || len(nodes) == 0 {
		return nil, nil
	}

	var (
		mu      sync.Mutex
		traffic []*xray.Traffic
		clients []*xray.ClientTraffic
		wg      sync.WaitGroup
	)
	var svc service.NodeService

	for _, n := range nodes {
		wg.Add(1)
		go func(id int, name string) {
			defer wg.Done()
			runner, err := svc.RunnerFor(id)
			if err != nil {
				svc.MarkResult(id, err)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), nodeTickTimeout)
			defer cancel()

			res, err := runner.Collect(ctx)
			svc.MarkResult(id, err)
			if err != nil {
				logger.Debugf("node %q: collect failed, its counters keep the bytes for the next tick: %v", name, err)
				return
			}
			mu.Lock()
			traffic = append(traffic, res.Traffics...)
			clients = append(clients, res.ClientTraffics...)
			mu.Unlock()
		}(n.Id, n.Name)
	}
	wg.Wait()
	return traffic, clients
}

// enforceOnNodes pushes the disabled set to every enabled remote node.
//
// Level-triggered: the whole set goes down every tick, so a node that missed one
// message self-corrects on the next rather than leaving an over-quota account
// running until it happens to reconnect.
func (j *XrayTrafficJob) enforceOnNodes(emails []string) {
	nodes, err := remoteNodes()
	if err != nil || len(nodes) == 0 {
		return
	}

	var svc service.NodeService
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(id int, name string) {
			defer wg.Done()
			runner, err := svc.RunnerFor(id)
			if err != nil {
				return // already recorded by the collect pass this tick
			}
			ctx, cancel := context.WithTimeout(context.Background(), nodeTickTimeout)
			defer cancel()
			if err := runner.Enforce(ctx, node.DisabledSet{Emails: emails}); err != nil {
				logger.Debugf("node %q: enforce failed: %v", name, err)
			}
		}(n.Id, n.Name)
	}
	wg.Wait()
}

// unionEmails merges the per-protocol disabled lists into one set.
//
// A node may serve an account on a protocol this master's own host does not run
// at all, so sending only the list that happened to fire locally would leave
// that account running there.
func unionEmails(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, e := range l {
			if seen[e] {
				continue
			}
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

// remoteNodes lists the enabled nodes that are not this host.
//
// The local node is excluded because the job already collects from and enforces
// on this machine inline; including it would bill every local account twice.
func remoteNodes() ([]model.Node, error) {
	var nodes []model.Node
	err := database.GetDB().
		Where("enable = ? AND is_local = ?", true, false).
		Find(&nodes).Error
	return nodes, err
}
