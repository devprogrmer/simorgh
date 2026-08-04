package main

import (
	"net"
	"time"
)

// transport moves raw envelope bytes between peers. Envelope framing
// (session id, sequence number, handshake/data payload) is built and parsed
// entirely in main.go / handshake.go - a transport just gets bytes from A to
// B, whatever the underlying wire format is.
type transport interface {
	Send(to net.Addr, data []byte) error
	Recv(buf []byte) (n int, from net.Addr, err error)
	ResolveAddr(host string) (net.Addr, error)
	SetReadDeadline(t time.Time) error
	Close() error
}

func newTransport(cfg *Config) (transport, error) {
	switch cfg.Transport {
	case "udp":
		return newUDPTransport(cfg)
	default:
		return newICMPTransport()
	}
}
