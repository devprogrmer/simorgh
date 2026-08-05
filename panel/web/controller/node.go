package controller

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/mhsanaei/3x-ui/v2/web/service"

	"github.com/gin-gonic/gin"
)

// NodeController is the node-management API.
//
// Every route is super-admin only. A node applies configuration as root on
// another machine and adding one takes SSH credentials, so this is escalation
// class — the same bar as the database export, the panel binary and a host
// reboot. A permission bit must not be enough to reach it.
type NodeController struct {
	nodeService      service.NodeService
	placementService service.PlacementService
}

func NewNodeController(g *gin.RouterGroup) *NodeController {
	a := &NodeController{}
	a.initRouter(g)
	return a
}

func (a *NodeController) initRouter(g *gin.RouterGroup) {
	g = g.Group("/nodes")
	g.Use(requireSuperAdmin())

	g.GET("/list", a.list)
	g.POST("/add", a.add)
	g.POST("/del/:id", a.del)
	g.GET("/status/:id", a.status)
	g.GET("/logs/:id/:core", a.logs)
	g.POST("/provision/:id", a.provision)

	// Placements: which machines serve a given inbound. Under the same
	// super-admin gate as the rest, because choosing where an inbound runs is
	// choosing which machine receives its credentials.
	g.GET("/placements/:inboundId", a.listPlacements)
	g.POST("/placements/:inboundId", a.setPlacements)
	g.POST("/placement/:id", a.updatePlacement)
}

func (a *NodeController) listPlacements(c *gin.Context) {
	inboundId, err := strconv.Atoi(c.Param("inboundId"))
	if err != nil {
		jsonMsg(c, "Placements", err)
		return
	}
	list, err := a.placementService.ListForInbound(inboundId)
	if err != nil {
		jsonMsg(c, "Placements", err)
		return
	}
	jsonObj(c, list, nil)
}

func (a *NodeController) setPlacements(c *gin.Context) {
	inboundId, err := strconv.Atoi(c.Param("inboundId"))
	if err != nil {
		jsonMsg(c, "Placements", err)
		return
	}
	var body struct {
		NodeIds []int `json:"nodeIds"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		jsonMsg(c, "Placements", err)
		return
	}
	if err := a.placementService.SetForInbound(inboundId, body.NodeIds); err != nil {
		jsonMsg(c, "Placements", err)
		return
	}
	jsonMsg(c, "Placements", nil)
}

func (a *NodeController) updatePlacement(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "Placement", err)
		return
	}
	var body struct {
		Port   int    `json:"port"`
		Listen string `json:"listen"`
		Enable bool   `json:"enable"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		jsonMsg(c, "Placement", err)
		return
	}
	if err := a.placementService.UpdatePlacement(id, body.Port, body.Listen, body.Enable); err != nil {
		jsonMsg(c, "Placement", err)
		return
	}
	jsonMsg(c, "Placement", nil)
}

func (a *NodeController) list(c *gin.Context) {
	nodes, err := a.nodeService.List()
	if err != nil {
		jsonMsg(c, "Nodes", err)
		return
	}
	jsonObj(c, nodes, nil)
}

// addRequest is what the Add-node form sends.
//
// The SSH credential is accepted, used once, and never stored — see
// node.Bootstrap. It is bound here rather than being a query parameter so it
// does not end up in an access log or a browser history entry.
type addRequest struct {
	Name       string `json:"name" form:"name"`
	Host       string `json:"host" form:"host"`
	SSHPort    int    `json:"sshPort" form:"sshPort"`
	User       string `json:"user" form:"user"`
	Password   string `json:"password" form:"password"`
	PrivateKey string `json:"privateKey" form:"privateKey"`
	APIPort    int    `json:"apiPort" form:"apiPort"`
}

func (a *NodeController) add(c *gin.Context) {
	var req addRequest
	if err := c.ShouldBind(&req); err != nil {
		jsonMsg(c, "Nodes", err)
		return
	}
	// Bootstrap can take a while: it uploads a binary of nearly a hundred
	// megabytes over a link that may be slow, then waits for systemd.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Minute)
	defer cancel()

	n, err := a.nodeService.Add(ctx, service.AddNodeRequest{
		Name:       req.Name,
		Host:       req.Host,
		SSHPort:    req.SSHPort,
		User:       req.User,
		Password:   req.Password,
		PrivateKey: req.PrivateKey,
		APIPort:    req.APIPort,
	})
	if err != nil {
		jsonMsg(c, "Nodes", err)
		return
	}
	jsonObj(c, n, nil)
}

func (a *NodeController) del(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "Nodes", err)
		return
	}
	jsonMsg(c, "Nodes", a.nodeService.Delete(id))
}

func (a *NodeController) status(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "Nodes", err)
		return
	}
	runner, err := a.nodeService.RunnerFor(id)
	if err != nil {
		jsonMsg(c, "Nodes", err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	st, err := runner.Status(ctx)
	// The outcome is recorded so a node an operator is looking at is also a node
	// the offline detector has heard from.
	a.nodeService.MarkResult(id, err)
	if err != nil {
		jsonMsg(c, "Nodes", err)
		return
	}
	jsonObj(c, st, nil)
}

func (a *NodeController) logs(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "Nodes", err)
		return
	}
	runner, err := a.nodeService.RunnerFor(id)
	if err != nil {
		jsonMsg(c, "Nodes", err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	out, err := runner.Logs(ctx, c.Param("core"))
	if err != nil {
		jsonMsg(c, "Nodes", err)
		return
	}
	jsonObj(c, out, nil)
}

// provision installs prerequisites on a node, streaming progress as
// newline-delimited JSON.
//
// Streamed rather than collected because provisioning can install a kernel and
// repoint the bootloader, which takes minutes; an operator watching a blank
// screen cannot tell that from a hang. This is the same ProvisionStep shape the
// local setup console already renders, so the UI needs no second code path.
func (a *NodeController) provision(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "Nodes", err)
		return
	}
	var body struct {
		Cores []string `json:"cores" form:"cores"`
	}
	_ = c.ShouldBind(&body)

	runner, err := a.nodeService.RunnerFor(id)
	if err != nil {
		jsonMsg(c, "Nodes", err)
		return
	}

	ch, err := runner.Provision(c.Request.Context(), body.Cores)
	if err != nil {
		jsonMsg(c, "Nodes", err)
		return
	}
	c.Header("Content-Type", "application/x-ndjson")
	c.Status(200)
	enc := json.NewEncoder(c.Writer)
	for step := range ch {
		if err := enc.Encode(step); err != nil {
			return // the operator navigated away
		}
		c.Writer.Flush()
	}
}
