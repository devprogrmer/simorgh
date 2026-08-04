package main

import (
	"fmt"
	"log"
	"strings"
)

// applyOperatingMode interprets OPERATING_MODE and configures the interface
// accordingly. Supported forms:
//
//	""                                        -> do nothing; the manager
//	                                              script assigns the address.
//	"ip:mask:srv_ip:cli_ip:dynamic:metric"    -> we assign the address
//	                                              ourselves (srv_ip on the
//	                                              FOREIGN server, cli_ip on
//	                                              the IRAN client) and,
//	                                              if metric is set, install a
//	                                              route with that metric.
//	                                              "dynamic" is reserved for a
//	                                              future DHCP-style mode and
//	                                              is currently a no-op.
//	"bridge:br0:br1"                          -> the interface (must be a
//	                                              TAP) is enslaved to br0 on
//	                                              the FOREIGN server side or
//	                                              br1 on the IRAN client
//	                                              side. The bridge itself
//	                                              must already exist.
func applyOperatingMode(cfg *Config, iface string, isClient bool) error {
	mode := strings.TrimSpace(cfg.OperatingMode)
	if mode == "" || mode == "none" {
		return nil
	}

	parts := strings.Split(mode, ":")
	switch parts[0] {
	case "ip":
		return applyIPMode(parts, iface, isClient)
	case "bridge":
		return applyBridgeMode(parts, iface, isClient)
	default:
		log.Printf("[netcfg] unrecognized OPERATING_MODE %q, ignoring", mode)
		return nil
	}
}

func applyIPMode(parts []string, iface string, isClient bool) error {
	if len(parts) < 4 {
		return fmt.Errorf("OPERATING_MODE ip: expected ip:mask:srv_ip:cli_ip[:dynamic:metric]")
	}
	mask := parts[1]
	srvIP := parts[2]
	cliIP := parts[3]
	var metric string
	if len(parts) >= 6 {
		metric = parts[5]
	}

	addr := srvIP
	if isClient {
		addr = cliIP
	}
	if addr == "" {
		log.Printf("[netcfg] ip mode: dynamic addressing requested but not implemented; skipping address assignment")
		return nil
	}

	if err := runIP("addr", "add", fmt.Sprintf("%s/%s", addr, mask), "dev", iface); err != nil {
		log.Printf("[netcfg] addr add: %v (continuing - may already be assigned)", err)
	}

	if metric != "" && metric != "dynamic" {
		peer := cliIP
		if isClient {
			peer = srvIP
		}
		if err := runIP("route", "replace", peer, "dev", iface, "metric", metric); err != nil {
			log.Printf("[netcfg] route replace: %v", err)
		}
	}
	return nil
}

func applyBridgeMode(parts []string, iface string, isClient bool) error {
	if len(parts) < 2 {
		return fmt.Errorf("OPERATING_MODE bridge: expected bridge:br0[:br1]")
	}
	brServer := parts[1]
	brClient := brServer
	if len(parts) >= 3 {
		brClient = parts[2]
	}
	bridge := brServer
	if isClient {
		bridge = brClient
	}
	if err := runIP("link", "set", "dev", iface, "master", bridge); err != nil {
		return fmt.Errorf("attach %s to bridge %s: %w", iface, bridge, err)
	}
	log.Printf("[netcfg] %s attached to bridge %s", iface, bridge)
	return nil
}
