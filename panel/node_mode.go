package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"

	"github.com/mhsanaei/3x-ui/v2/config"
	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/logger"
	"github.com/mhsanaei/3x-ui/v2/node"
	"github.com/mhsanaei/3x-ui/v2/web/service"
)

// Node mode: the same binary, serving one machine to a master panel instead of
// serving a web UI to people.
//
// It starts no web server, no cron scheduler and no Telegram bot -- a node has
// no users, no jobs of its own and nothing to notify. It DOES open a database,
// which is not the same as being a source of truth: every protocol service
// reads its inbounds through database.GetDB(), so the node materialises the
// state it is pushed into that schema and the existing drivers work unchanged.
// The master owns the accounts, the ledger and the traffic history; the node
// holds a working copy of what it must currently serve.

// nodeConfig is written by the master during SSH bootstrap and read here.
type nodeConfig struct {
	Listen string `json:"listen"` // host:port to serve on, e.g. "0.0.0.0:62050"
	CACert string `json:"caCert"` // PEM; the master's CA -- the only accepted client issuer
	Cert   string `json:"cert"`   // PEM; this node's server certificate
	Key    string `json:"key"`    // PEM; its private key
}

const defaultNodeConfigPath = "/etc/simorgh-node.json"
const defaultNodeListen = "0.0.0.0:62050"

// runNodeMode serves the node API until killed.
func runNodeMode(args []string) {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	cfgPath := fs.String("config", defaultNodeConfigPath, "path to the node config written by the master")
	listen := fs.String("listen", "", "override the listen address from the config")
	if err := fs.Parse(args); err != nil {
		return
	}

	cfg, err := loadNodeConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "node:", err)
		fmt.Fprintln(os.Stderr, "A node is set up by the master panel (Nodes -> Add node), which writes this file.")
		os.Exit(1)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if cfg.Listen == "" {
		cfg.Listen = defaultNodeListen
	}

	if err := database.InitDB(config.GetDBPath()); err != nil {
		fmt.Fprintln(os.Stderr, "node: cannot open the local database:", err)
		os.Exit(1)
	}

	tlsCfg, err := node.NodeTLSConfig([]byte(cfg.CACert), []byte(cfg.Cert), []byte(cfg.Key))
	if err != nil {
		// Refusing to start is the point. Serving this API without verifying the
		// client would hand a root-level configuration channel to anyone who can
		// reach the port, so there is no degraded mode to fall back to.
		fmt.Fprintln(os.Stderr, "node: certificate material is unusable:", err)
		os.Exit(1)
	}

	ln, err := tls.Listen("tcp", cfg.Listen, tlsCfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "node: cannot listen on", cfg.Listen+":", err)
		os.Exit(1)
	}
	defer ln.Close()

	srv := &http.Server{Handler: node.NewServer(service.NewLocalRunner()).Handler()}
	logger.Info("node: serving " + cfg.Listen + " (mTLS, master CA only)")
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "node:", err)
		os.Exit(1)
	}
}

func loadNodeConfig(path string) (nodeConfig, error) {
	var cfg nodeConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("cannot read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if cfg.CACert == "" || cfg.Cert == "" || cfg.Key == "" {
		return cfg, fmt.Errorf("%s is missing certificate material", path)
	}
	if cfg.Listen != "" {
		if _, p, err := net.SplitHostPort(cfg.Listen); err != nil {
			return cfg, fmt.Errorf("listen %q is not host:port: %w", cfg.Listen, err)
		} else if _, err := strconv.Atoi(p); err != nil {
			return cfg, fmt.Errorf("listen %q has a non-numeric port", cfg.Listen)
		}
	}
	return cfg, nil
}
