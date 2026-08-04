package main

import (
	"fmt"
	"net"
	"time"
)

// udpTransport is the fallback transport for networks/firewalls that block
// raw ICMP outright. Framing is trivial - envelope bytes go straight into
// UDP datagrams. Dual-stack: binding "udp" (rather than "udp4") on an
// unspecified address makes Linux accept both IPv4 and IPv6 on the same
// socket, and ResolveAddr/net.ResolveUDPAddr with network "udp" handles
// either family of host transparently.
type udpTransport struct {
	conn *net.UDPConn
	port int
}

func newUDPTransport(cfg *Config) (*udpTransport, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: cfg.UDPPort})
	if err != nil {
		return nil, fmt.Errorf("listen udp :%d: %w", cfg.UDPPort, err)
	}
	return &udpTransport{conn: conn, port: cfg.UDPPort}, nil
}

func (t *udpTransport) Send(to net.Addr, data []byte) error {
	addr, ok := to.(*net.UDPAddr)
	if !ok {
		return fmt.Errorf("udp transport requires *net.UDPAddr")
	}
	_, err := t.conn.WriteToUDP(data, addr)
	return err
}

func (t *udpTransport) Recv(buf []byte) (int, net.Addr, error) {
	n, from, err := t.conn.ReadFromUDP(buf)
	return n, from, err
}

func (t *udpTransport) ResolveAddr(host string) (net.Addr, error) {
	return net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", host, t.port))
}

func (t *udpTransport) SetReadDeadline(dl time.Time) error {
	return t.conn.SetReadDeadline(dl)
}

func (t *udpTransport) Close() error { return t.conn.Close() }
