package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/node"
)

// NodeService owns the fleet: which machines exist, how to reach them, and
// whether they are answering.
//
// Like the other services it is a zero-value-usable value type whose methods are
// stateless (they read the database and dial), so a fresh copy works.
type NodeService struct{}

// Settings keys for the master's certificate authority. Stored in the settings
// table rather than on disk so a database backup carries the fleet's identity
// with it: a restore that brought the nodes back but not the CA would leave the
// panel unable to talk to any of them.
const (
	nodeCACertKey = "nodeCACert"
	nodeCAKeyKey  = "nodeCAKey"
)

var (
	ErrNodeNotFound      = errors.New("node not found")
	ErrCannotDeleteLocal = errors.New("the local node represents this panel's own host and cannot be removed")
	ErrNodeUnreachable   = errors.New("node has no certificate material yet; finish adding it first")
	// errTestDial exists so tests can drive MarkResult without a network.
	errTestDial = errors.New("dial failed")
)

// ensureCA returns the master's certificate authority, generating it once.
//
// Generating a second one would invalidate every node certificate already
// issued and take the whole fleet offline at the same moment, so this is
// deliberately read-then-create rather than create-if-absent-on-every-call.
func (s *NodeService) ensureCA() (certPEM, keyPEM []byte, err error) {
	var setting SettingService
	cert, _ := setting.getString(nodeCACertKey)
	key, _ := setting.getString(nodeCAKeyKey)
	if cert != "" && key != "" {
		return []byte(cert), []byte(key), nil
	}

	c, k, err := node.GenerateCA()
	if err != nil {
		return nil, nil, err
	}
	if err := setting.setString(nodeCACertKey, string(c)); err != nil {
		return nil, nil, err
	}
	if err := setting.setString(nodeCAKeyKey, string(k)); err != nil {
		return nil, nil, err
	}
	logger.Info("node: generated the master certificate authority")
	return c, k, nil
}

// List returns the fleet with key material stripped.
//
// The stripping is belt-and-braces: Node already carries json:"-" on the
// certificate fields. Both are kept because this is the method the API returns,
// and a node's private key reaching an admin's browser is a node an attacker can
// impersonate to the panel.
func (s *NodeService) List() ([]model.Node, error) {
	var nodes []model.Node
	if err := database.GetDB().Order("is_local DESC, id ASC").Find(&nodes).Error; err != nil {
		return nil, err
	}
	for i := range nodes {
		nodes[i].ServerCert = ""
		nodes[i].ServerKey = ""
	}
	return nodes, nil
}

func (s *NodeService) Get(id int) (*model.Node, error) {
	var n model.Node
	if err := database.GetDB().First(&n, id).Error; err != nil {
		return nil, ErrNodeNotFound
	}
	return &n, nil
}

// RunnerFor returns how to drive one node.
//
// The local node is driven in-process; anything else goes over mTLS. A remote
// node with no certificate yet is an ERROR rather than a fall back to the local
// runner, which would apply another machine's configuration to this one.
func (s *NodeService) RunnerFor(id int) (node.Runner, error) {
	n, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if n.IsLocal {
		return NewLocalRunner(), nil
	}
	if n.ServerCert == "" || n.Address == "" {
		return nil, ErrNodeUnreachable
	}
	caCert, caKey, err := s.ensureCA()
	if err != nil {
		return nil, err
	}
	// The master's client certificate is minted per call rather than stored. It
	// is cheap (one P-256 key), it keeps one less secret in the database, and it
	// means a compromised backup does not hand over a credential that is valid
	// against every node until its expiry.
	cliCert, cliKey, err := node.IssueMasterCert(caCert, caKey)
	if err != nil {
		return nil, err
	}
	return node.NewClient(node.ClientConfig{
		Address: n.Address,
		Port:    n.APIPort,
		CACert:  caCert,
		Cert:    cliCert,
		Key:     cliKey,
	})
}

// MarkResult records the outcome of one contact with a node.
//
// A node goes offline only after NodeOfflineAfterFailures consecutive failures.
// One slow tick on a link to another country is not an outage, and flapping a
// node's status flaps every inbound on it in the UI. A success clears the
// streak, so a node that failed twice hours ago is not one blip from being
// declared down.
func (s *NodeService) MarkResult(id int, err error) {
	db := database.GetDB()
	var n model.Node
	if e := db.First(&n, id).Error; e != nil {
		return
	}

	if err == nil {
		updates := map[string]any{
			"status":       model.NodeOnline,
			"failed_ticks": 0,
			"last_error":   "",
			"last_seen":    time.Now().Unix(),
		}
		db.Model(&n).Updates(updates)
		return
	}

	failed := n.FailedTicks + 1
	updates := map[string]any{
		"failed_ticks": failed,
		"last_error":   err.Error(),
	}
	if failed >= model.NodeOfflineAfterFailures {
		updates["status"] = model.NodeOffline
		if n.Status != model.NodeOffline {
			logger.Warningf("node %q offline after %d consecutive failures: %v", n.Name, failed, err)
		}
	}
	db.Model(&n).Updates(updates)
}

// Delete removes a node.
//
// It refuses while inbounds are still placed there, and names them. Silently
// orphaning a placement would leave those accounts served by nothing, with the
// panel showing an inbound that exists and works as far as it knows.
func (s *NodeService) Delete(id int) error {
	n, err := s.Get(id)
	if err != nil {
		return err
	}
	if n.IsLocal {
		return ErrCannotDeleteLocal
	}

	db := database.GetDB()
	var placements []model.InboundNode
	if err := db.Where("node_id = ?", id).Find(&placements).Error; err != nil {
		return err
	}
	if len(placements) > 0 {
		tags := make([]string, 0, len(placements))
		for _, p := range placements {
			var in model.Inbound
			if db.Select("tag").First(&in, p.InboundId).Error == nil {
				tags = append(tags, in.Tag)
			}
		}
		return fmt.Errorf("this node still serves %d inbound(s) (%s); move or remove them first",
			len(placements), strings.Join(tags, ", "))
	}
	return db.Delete(&model.Node{}, id).Error
}

// Tick drives every enabled node once, in parallel.
//
// Parallel because one unreachable node in another country would otherwise stall
// the tick for every healthy one behind it: with a 30s client timeout and five
// nodes, a single dead node turns a two-second tick into half a minute, and
// traffic accounting is what waits.
//
// Each node gets its own timeout so a slow one cannot hold the group open.
func (s *NodeService) Tick(ctx context.Context, fn func(context.Context, int, node.Runner) error) {
	var nodes []model.Node
	if err := database.GetDB().Where("enable = ?", true).Find(&nodes).Error; err != nil {
		logger.Warning("node tick: cannot list nodes:", err)
		return
	}

	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			runner, err := s.RunnerFor(id)
			if err != nil {
				s.MarkResult(id, err)
				return
			}
			nodeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			s.MarkResult(id, fn(nodeCtx, id, runner))
		}(n.Id)
	}
	wg.Wait()
}
