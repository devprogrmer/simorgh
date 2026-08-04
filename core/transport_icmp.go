package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"time"
)

const (
	icmpEchoRequest   = 8   // ICMPv4 echo request
	icmpv6EchoRequest = 128 // ICMPv6 echo request
	icmpHeaderLen     = 8   // type, code, checksum(2), kernel-id(2), kernel-seq(2) - same layout both families
)

func icmpChecksum(data []byte) uint16 {
	var sum uint32
	n := len(data)
	for i := 0; i+1 < n; i += 2 {
		sum += uint32(data[i])<<8 | uint32(data[i+1])
	}
	if n%2 == 1 {
		sum += uint32(data[n-1]) << 8
	}
	for sum>>16 > 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

type icmpRecvResult struct {
	data []byte
	from net.Addr
}

// icmpTransport carries envelope bytes as the payload of ICMP echo-request
// (IPv4) or ICMPv6 echo-request (IPv6) messages. Only echo *requests* are
// ever treated as data by either peer - this is what stops each host's own
// kernel from silently auto-replying and having that reply misread as
// data. Dual-stack: it opens both an IPv4 and an IPv6 raw socket where
// available and merges their traffic, so REMOTE_IP can be either family and
// a server can be reached over whichever family its client used.
type icmpTransport struct {
	v4     *net.IPConn
	v6     *net.IPConn
	recvCh chan icmpRecvResult
	errCh  chan error
}

func newICMPTransport() (*icmpTransport, error) {
	t := &icmpTransport{recvCh: make(chan icmpRecvResult, 64), errCh: make(chan error, 4)}

	v4conn, err4 := net.ListenPacket("ip4:1", "0.0.0.0")
	if err4 == nil {
		t.v4 = v4conn.(*net.IPConn)
		go t.readLoop(t.v4, false)
	}

	v6conn, err6 := net.ListenPacket("ip6:58", "::")
	if err6 == nil {
		t.v6 = v6conn.(*net.IPConn)
		go t.readLoop(t.v6, true)
	}

	if t.v4 == nil && t.v6 == nil {
		return nil, fmt.Errorf("could not open a raw ICMP socket for IPv4 (%v) or IPv6 (%v) - need CAP_NET_RAW", err4, err6)
	}
	if t.v4 == nil {
		log.Printf("[transport] IPv4 ICMP unavailable (%v) - IPv6 only", err4)
	}
	if t.v6 == nil {
		log.Printf("[transport] IPv6 ICMPv6 unavailable (%v) - IPv4 only", err6)
	}
	return t, nil
}

func (t *icmpTransport) readLoop(conn *net.IPConn, v6 bool) {
	raw := make([]byte, 66000)
	for {
		n, from, err := conn.ReadFrom(raw)
		if err != nil {
			t.errCh <- err
			return
		}

		var icmp []byte
		if v6 {
			// Linux ICMPv6 raw sockets deliver the message without an IPv6
			// header ahead of it.
			if n < icmpHeaderLen {
				continue
			}
			icmp = raw[:n]
			if icmp[0] != icmpv6EchoRequest {
				continue
			}
		} else {
			if n < icmpHeaderLen {
				continue
			}
			candidate := raw[:n]
			// Some kernels/Go runtimes prepend the IPv4 header on a raw
			// socket read, some don't - detect it by checking for a
			// plausible version+IHL byte (0x45-0x4f); our own ICMP type
			// byte (8) never falls in that range.
			if n >= 20 && raw[0]>>4 == 4 {
				ihl := int(raw[0]&0x0f) * 4
				if ihl >= 20 && n >= ihl+icmpHeaderLen {
					candidate = raw[ihl:n]
				}
			}
			if candidate[0] != icmpEchoRequest {
				continue
			}
			icmp = candidate
		}

		payload := append([]byte(nil), icmp[icmpHeaderLen:]...)
		t.recvCh <- icmpRecvResult{data: payload, from: from}
	}
}

func (t *icmpTransport) Send(to net.Addr, data []byte) error {
	addr, ok := to.(*net.IPAddr)
	if !ok {
		return fmt.Errorf("icmp transport requires *net.IPAddr")
	}

	if addr.IP.To4() == nil {
		if t.v6 == nil {
			return fmt.Errorf("no IPv6 ICMP socket available (peer address is IPv6)")
		}
		msg := make([]byte, icmpHeaderLen+len(data))
		msg[0] = icmpv6EchoRequest
		msg[1] = 0
		// checksum left at 0: the kernel computes and fills the ICMPv6
		// checksum automatically for IPPROTO_ICMPV6 raw sockets.
		copy(msg[icmpHeaderLen:], data)
		_, err := t.v6.WriteTo(msg, addr)
		return err
	}

	if t.v4 == nil {
		return fmt.Errorf("no IPv4 ICMP socket available (peer address is IPv4)")
	}
	msg := make([]byte, icmpHeaderLen+len(data))
	msg[0] = icmpEchoRequest
	msg[1] = 0
	binary.BigEndian.PutUint16(msg[4:6], 0)
	binary.BigEndian.PutUint16(msg[6:8], 0)
	copy(msg[icmpHeaderLen:], data)
	cs := icmpChecksum(msg)
	binary.BigEndian.PutUint16(msg[2:4], cs)
	_, err := t.v4.WriteTo(msg, addr)
	return err
}

func (t *icmpTransport) Recv(buf []byte) (int, net.Addr, error) {
	select {
	case res := <-t.recvCh:
		if len(res.data) > len(buf) {
			return 0, nil, fmt.Errorf("packet too large for buffer")
		}
		copy(buf, res.data)
		return len(res.data), res.from, nil
	case err := <-t.errCh:
		return 0, nil, err
	}
}

func (t *icmpTransport) ResolveAddr(host string) (net.Addr, error) {
	return net.ResolveIPAddr("ip", host) // resolves either address family
}

func (t *icmpTransport) SetReadDeadline(dl time.Time) error {
	// Recv() merges both families through a channel rather than reading a
	// single conn directly, so a per-conn deadline doesn't apply here.
	// Nothing in this codebase currently relies on it (handshake timeouts
	// use their own time.After select instead) - kept as a harmless no-op
	// to satisfy the transport interface.
	return nil
}

func (t *icmpTransport) Close() error {
	var err error
	if t.v4 != nil {
		if e := t.v4.Close(); e != nil {
			err = e
		}
	}
	if t.v6 != nil {
		if e := t.v6.Close(); e != nil {
			err = e
		}
	}
	return err
}

// setOutgoingDSCP sets the DSCP bits (upper 6 bits of the IPv4 TOS byte) on
// every packet the IPv4 socket sends. IPv6 traffic class marking would need
// its own sockopt (IPV6_TCLASS) - not wired up yet, since DSCP_MARK is
// documented as an IPv4-path feature.
func setOutgoingDSCP(t *icmpTransport, dscp int) error {
	if dscp < 0 || t.v4 == nil {
		return nil
	}
	tos := (dscp & 0x3f) << 2
	rawConn, err := t.v4.SyscallConn()
	if err != nil {
		return err
	}
	var sockErr error
	err = rawConn.Control(func(fd uintptr) {
		sockErr = setsockoptIPTOS(int(fd), tos)
	})
	if err != nil {
		return err
	}
	return sockErr
}
