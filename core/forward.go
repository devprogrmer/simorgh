package main

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// udpClientForwardSink runs on the IRAN side in "forward" mode. It listens
// on a local UDP port that you point your WireGuard/OpenVPN *client* at
// (its Endpoint / remote), learns that client's address from its first
// packet, and relays everything both ways through the Simorgh tunnel.
type udpClientForwardSink struct {
	conn *net.UDPConn
	app  atomic.Pointer[net.UDPAddr]
}

func newUDPClientForwardSink(bindAddr string, port int) (*udpClientForwardSink, error) {
	ip := net.ParseIP(bindAddr)
	if ip == nil {
		return nil, fmt.Errorf("invalid FORWARD_BIND %q", bindAddr)
	}
	network := "udp4"
	if ip.To4() == nil {
		network = "udp6"
	}
	conn, err := net.ListenUDP(network, &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		return nil, fmt.Errorf("listen udp %s:%d: %w", bindAddr, port, err)
	}
	return &udpClientForwardSink{conn: conn}, nil
}

func (s *udpClientForwardSink) ReadFrame(buf []byte) (int, error) {
	n, addr, err := s.conn.ReadFromUDP(buf)
	if err != nil {
		return 0, err
	}
	s.app.Store(addr)
	return n, nil
}

func (s *udpClientForwardSink) WriteFrame(frame []byte) error {
	app := s.app.Load()
	if app == nil {
		return nil // local app (WireGuard/OpenVPN client) hasn't sent anything yet
	}
	_, err := s.conn.WriteToUDP(frame, app)
	return err
}

func (s *udpClientForwardSink) Close() error { return s.conn.Close() }

// udpServerForwardSink runs on the FOREIGN side in "forward" mode. It
// relays tunnel payloads to a local target service (your actual
// WireGuard/OpenVPN *server*, normally on 127.0.0.1) and carries its
// replies back through the tunnel.
type udpServerForwardSink struct {
	conn   *net.UDPConn
	target *net.UDPAddr
}

func newUDPServerForwardSink(targetHost string, targetPort int) (*udpServerForwardSink, error) {
	target, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", targetHost, targetPort))
	if err != nil {
		return nil, fmt.Errorf("resolve TARGET_HOST/TARGET_PORT: %w", err)
	}
	network := "udp4"
	if target.IP.To4() == nil {
		network = "udp6"
	}
	conn, err := net.ListenUDP(network, nil)
	if err != nil {
		return nil, fmt.Errorf("open relay udp socket: %w", err)
	}
	return &udpServerForwardSink{conn: conn, target: target}, nil
}

func (s *udpServerForwardSink) ReadFrame(buf []byte) (int, error) {
	n, _, err := s.conn.ReadFromUDP(buf) // we only ever talk to s.target
	return n, err
}

func (s *udpServerForwardSink) WriteFrame(frame []byte) error {
	_, err := s.conn.WriteToUDP(frame, s.target)
	return err
}

func (s *udpServerForwardSink) Close() error { return s.conn.Close() }

// tcpClientForwardSink runs on the IRAN side when FORWARD_PROTO=tcp. It
// listens on a local TCP port and accepts one connection at a time (a new
// connection replaces whatever was there) - point your VPN client's
// TCP-mode endpoint at it.
//
// Note on correctness: this proxies bytes across two independently
// terminated TCP connections through Simorgh's own (lossy, datagram-based)
// tunnel in between. FEC recovers isolated frame loss, but an uncorrected
// loss will corrupt the byte stream - the affected connection will need to
// reconnect, the same as it would over any unreliable link. This is not a
// fully reliable relay; enable FEC_ENABLE for anything beyond casual use.
type tcpClientForwardSink struct {
	ln     net.Listener
	connMu sync.Mutex
	conn   net.Conn
}

func newTCPClientForwardSink(bindAddr string, port int) (*tcpClientForwardSink, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bindAddr, port))
	if err != nil {
		return nil, fmt.Errorf("listen tcp %s:%d: %w", bindAddr, port, err)
	}
	s := &tcpClientForwardSink{ln: ln}
	go s.acceptLoop()
	return s, nil
}

func (s *tcpClientForwardSink) acceptLoop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.connMu.Lock()
		if s.conn != nil {
			s.conn.Close()
		}
		s.conn = c
		s.connMu.Unlock()
	}
}

func (s *tcpClientForwardSink) ReadFrame(buf []byte) (int, error) {
	for {
		s.connMu.Lock()
		c := s.conn
		s.connMu.Unlock()
		if c == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		n, err := c.Read(buf)
		if err != nil {
			s.connMu.Lock()
			if s.conn == c {
				s.conn = nil
			}
			s.connMu.Unlock()
			return 0, err // surface it - the caller needs to know this leg closed
		}
		return n, nil
	}
}

func (s *tcpClientForwardSink) WriteFrame(frame []byte) error {
	s.connMu.Lock()
	c := s.conn
	s.connMu.Unlock()
	if c == nil {
		return nil // nobody connected locally right now - drop
	}
	_, err := c.Write(frame)
	return err
}

func (s *tcpClientForwardSink) Close() error { return s.ln.Close() }

// CloseCurrent closes just the currently-accepted local connection (not the
// listener) - used when the peer signals (pktClose) that their side of this
// proxied TCP connection has ended, so ours reflects that instead of
// hanging forever waiting for data that will never come.
func (s *tcpClientForwardSink) CloseCurrent() error {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conn != nil {
		err := s.conn.Close()
		s.conn = nil
		return err
	}
	return nil
}

// tcpServerForwardSink runs on the FOREIGN side when FORWARD_PROTO=tcp. It
// lazily dials the real target on first data and relays both ways.
type tcpServerForwardSink struct {
	target string
	connMu sync.Mutex
	conn   net.Conn
}

func newTCPServerForwardSink(targetHost string, targetPort int) (*tcpServerForwardSink, error) {
	return &tcpServerForwardSink{target: fmt.Sprintf("%s:%d", targetHost, targetPort)}, nil
}

func (s *tcpServerForwardSink) getConn() (net.Conn, error) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conn != nil {
		return s.conn, nil
	}
	c, err := net.DialTimeout("tcp", s.target, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", s.target, err)
	}
	s.conn = c
	return c, nil
}

func (s *tcpServerForwardSink) ReadFrame(buf []byte) (int, error) {
	for {
		s.connMu.Lock()
		c := s.conn
		s.connMu.Unlock()
		if c == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		n, err := c.Read(buf)
		if err != nil {
			s.connMu.Lock()
			if s.conn == c {
				s.conn = nil
			}
			s.connMu.Unlock()
			return 0, err
		}
		return n, nil
	}
}

func (s *tcpServerForwardSink) WriteFrame(frame []byte) error {
	c, err := s.getConn()
	if err != nil {
		return err
	}
	if _, err := c.Write(frame); err != nil {
		s.connMu.Lock()
		if s.conn == c {
			s.conn = nil
		}
		s.connMu.Unlock()
		return err
	}
	return nil
}

func (s *tcpServerForwardSink) Close() error {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}

// CloseCurrent closes the current dial to the target, if any - a fresh one
// is dialed lazily on the next WriteFrame. See tcpClientForwardSink's
// CloseCurrent for why this exists.
func (s *tcpServerForwardSink) CloseCurrent() error {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conn != nil {
		err := s.conn.Close()
		s.conn = nil
		return err
	}
	return nil
}
