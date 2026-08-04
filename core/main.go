// Command simorgh-core is the tunnel data-plane for the Simorgh gaming
// tunnel. It is dependency-free (Go standard library only): it opens a TUN
// or TAP device, negotiates a fresh per-session AES-256-GCM key via an
// X25519 handshake authenticated by a shared password, and carries the
// ciphertext either inside ICMP echo-request packets or plain UDP
// datagrams. Both the IRAN client and the FOREIGN server run this exact
// same binary; the role is decided purely by which environment variables
// are present (REMOTE_IP => client, otherwise => server).
package main

import (
	"crypto/ecdh"
	"encoding/binary"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxSinkReadSize bounds how much a single sink.ReadFrame() call may return.
// TUN frames and UDP forward datagrams are already naturally bounded by MTU
// or the sending application, but a TCP forward sink's Read() has no such
// bound on its own - without this cap, a bulk TCP transfer would produce
// frames too large for a single ICMP/UDP wire packet ("message too long").
// Kept safely under fecSlotSize (2048) too.
const maxSinkReadSize = 1400

// session holds the negotiated, forward-secret crypto state and the peer
// address to send to. A brand new one replaces the old one on every
// handshake.
type session struct {
	crypt *sessionCrypto
	peer  net.Addr
}

type pendingHandshake struct {
	priv      *ecdh.PrivateKey
	clientPub []byte
	resultCh  chan *session
}

func main() {
	log.SetFlags(0)
	log.SetOutput(os.Stdout)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("[fatal] config: %v", err)
	}

	role := "FOREIGN (server)"
	if cfg.IsClient {
		role = "IRAN (client)"
	}
	log.Printf("[simorgh] starting as %s, mode=%s transport=%s mtu=%d fec=%v",
		role, cfg.Mode, cfg.Transport, cfg.MTU, cfg.FECEnable)

	if !cfg.IsClient && cfg.CustomersFile != "" {
		customers, err := loadCustomers(cfg.CustomersFile)
		if err != nil {
			log.Fatalf("[fatal] %v", err)
		}
		tr, err := newTransport(cfg)
		if err != nil {
			log.Fatalf("[fatal] transport: %v", err)
		}
		defer tr.Close()
		runServerMulti(cfg, tr, customers)
		return
	}

	var sink dataSink
	if cfg.Mode == "forward" {
		if cfg.IsClient {
			var s dataSink
			var err error
			if cfg.ForwardProto == "tcp" {
				s, err = newTCPClientForwardSink(cfg.ForwardBind, cfg.LocalPort)
			} else {
				s, err = newUDPClientForwardSink(cfg.ForwardBind, cfg.LocalPort)
			}
			if err != nil {
				log.Fatalf("[fatal] forward listener: %v", err)
			}
			log.Printf("[simorgh] forward mode (%s): point your VPN client's endpoint at %s:%d", cfg.ForwardProto, cfg.ForwardBind, cfg.LocalPort)
			sink = s
		} else {
			var s dataSink
			var err error
			if cfg.ForwardProto == "tcp" {
				s, err = newTCPServerForwardSink(cfg.TargetHost, cfg.TargetPort)
			} else {
				s, err = newUDPServerForwardSink(cfg.TargetHost, cfg.TargetPort)
			}
			if err != nil {
				log.Fatalf("[fatal] forward relay: %v", err)
			}
			log.Printf("[simorgh] forward mode (%s): relaying to %s:%d", cfg.ForwardProto, cfg.TargetHost, cfg.TargetPort)
			sink = s
		}
	} else {
		useTap := len(cfg.OperatingMode) >= 6 && cfg.OperatingMode[:6] == "bridge"
		tun, ifaceName, err := openTunTap(cfg.Interface, useTap)
		if err != nil {
			log.Fatalf("[fatal] tun/tap: %v", err)
		}
		if err := setLinkMAC(ifaceName, cfg.MAC); err != nil {
			log.Printf("[warn] set MAC: %v", err)
		}
		if err := setLinkMTU(ifaceName, cfg.MTU); err != nil {
			log.Printf("[warn] set MTU: %v", err)
		}
		if err := setLinkUp(ifaceName); err != nil {
			log.Printf("[warn] set link up: %v", err)
		}
		if err := applyOperatingMode(cfg, ifaceName, cfg.IsClient); err != nil {
			log.Printf("[warn] operating mode: %v", err)
		}
		sink = &tunSink{f: tun}
	}
	defer sink.Close()

	var servers []string
	if cfg.IsClient {
		for _, h := range strings.Split(cfg.RemoteIP, ",") {
			h = strings.TrimSpace(h)
			if h != "" {
				servers = append(servers, h)
			}
		}
		if len(servers) == 0 {
			log.Fatalf("[fatal] REMOTE_IP is empty")
		}
	}

	var tr transport
	if cfg.IsClient && cfg.Transport == "auto" {
		tr = detectTransport(cfg, servers) // blocks until one works; also updates cfg.Transport to whichever it picked
	} else {
		tr, err = newTransport(cfg)
		if err != nil {
			log.Fatalf("[fatal] transport: %v", err)
		}
	}
	defer tr.Close()

	if cfg.IsClient && cfg.Transport == "icmp" && cfg.DSCPMark >= 0 {
		if it, ok := tr.(*icmpTransport); ok {
			if err := setOutgoingDSCP(it, cfg.DSCPMark); err != nil {
				log.Printf("[warn] DSCP mark: %v", err)
			}
		}
	}

	if cfg.IsClient {
		runClient(cfg, tr, sink, servers)
	} else {
		runServer(cfg, tr, sink)
	}
}

// detectTransport implements TRANSPORT=auto for the client: it tries ICMP
// first, then UDP, against every configured server, and locks onto
// whichever one actually gets a handshake reply. This exists for links
// that go unstable in ways that specifically break one transport but not
// the other (a path that suddenly starts dropping ICMP, for instance) -
// the client finds a working path itself instead of the operator having
// to guess and hardcode one. For this to succeed, the server side needs to
// actually be reachable on whichever transport wins - see
// docs/DEPLOYMENT.md for how to run both simultaneously.
func detectTransport(cfg *Config, servers []string) transport {
	order := []string{"icmp", "udp"}
	for {
		for _, kind := range order {
			log.Printf("[transport] auto-detect: trying %s...", kind)
			trial := *cfg
			trial.Transport = kind
			probeTr, err := newTransport(&trial)
			if err != nil {
				log.Printf("[transport] %s unavailable: %v", kind, err)
				continue
			}
			ok := probeAnyServer(&trial, probeTr, servers)
			probeTr.Close()
			if !ok {
				continue
			}
			log.Printf("[transport] auto-detect: %s reached a server - using it", kind)
			finalTr, err := newTransport(&trial)
			if err != nil {
				log.Printf("[transport] failed to reopen %s: %v", kind, err)
				continue
			}
			cfg.Transport = kind
			return finalTr
		}
		log.Printf("[transport] auto-detect: neither icmp nor udp reached any server, retrying in 3s...")
		time.Sleep(3 * time.Second)
	}
}

// probeAnyServer runs one disposable handshake attempt per candidate server
// on the given transport and reports whether any of them answered. The
// resulting session is discarded either way - this is purely a reachability
// probe, the real session is established fresh afterward.
func probeAnyServer(cfg *Config, tr transport, servers []string) bool {
	sessID := sessionIDFromPassword(cfg.Password)
	var pendingMap sync.Map
	var seqCounter uint32

	stop := make(chan struct{})
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := tr.Recv(buf)
			if err != nil {
				return // transport closed - the caller is done with us
			}
			select {
			case <-stop:
				return
			default:
			}
			if n < 4 || binary.BigEndian.Uint16(buf[0:2]) != sessID {
				continue
			}
			body := buf[4:n]
			if len(body) > 0 && body[0] == msgHelloAck {
				completeHandshake(&pendingMap, cfg.Password, from, body)
			}
		}
	}()
	defer close(stop)

	for _, h := range servers {
		addr, err := tr.ResolveAddr(h)
		if err != nil {
			continue
		}
		if _, _, err := attemptHandshake(tr, sessID, cfg.Password, addr, &pendingMap, &seqCounter, 3*time.Second); err == nil {
			return true
		}
	}
	return false
}

// ---- envelope helpers ------------------------------------------------

func buildEnvelope(sessID, seq uint16, body []byte) []byte {
	env := make([]byte, 4+len(body))
	binary.BigEndian.PutUint16(env[0:2], sessID)
	binary.BigEndian.PutUint16(env[2:4], seq)
	copy(env[4:], body)
	return env
}

func nextSeq(counter *uint32) uint16 {
	return uint16(atomic.AddUint32(counter, 1))
}

// ---- shared data-plane helpers ---------------------------------------

func sendData(tr transport, sessID uint16, current *atomic.Pointer[session], seqCounter *uint32, frame []byte, fecEnc *fecEncoder, bucket *tokenBucket) {
	sess := current.Load()
	if sess == nil {
		return // no session yet (server hasn't been handshaken with, or client mid-handshake)
	}

	if fecEnc != nil {
		groupID, index, complete := fecEnc.add(frame)
		header := make([]byte, 7, 7+len(frame))
		header[0] = pktData
		binary.BigEndian.PutUint32(header[1:5], groupID)
		header[5] = byte(index)
		header[6] = byte(fecEnc.GroupSize())
		plain := append(header, frame...)
		sealed := sess.crypt.seal(plain)
		if bucket != nil {
			bucket.wait(len(sealed))
		}
		envelope := buildEnvelope(sessID, nextSeq(seqCounter), sealed)
		if err := tr.Send(sess.peer, envelope); err != nil {
			log.Printf("[error] send data: %v", err)
		}

		if complete {
			gid, gsize, parity := fecEnc.flushParity()
			phdr := make([]byte, 6, 6+len(parity))
			phdr[0] = pktParity
			binary.BigEndian.PutUint32(phdr[1:5], gid)
			phdr[5] = byte(gsize)
			pplain := append(phdr, parity...)
			psealed := sess.crypt.seal(pplain)
			if bucket != nil {
				bucket.wait(len(psealed))
			}
			penv := buildEnvelope(sessID, nextSeq(seqCounter), psealed)
			if err := tr.Send(sess.peer, penv); err != nil {
				log.Printf("[error] send parity: %v", err)
			}
		}
		return
	}

	header := []byte{pktData, 0, 0, 0, 0, 0, 0} // groupSize=0 => FEC not in use
	plain := append(header, frame...)
	sealed := sess.crypt.seal(plain)
	if bucket != nil {
		bucket.wait(len(sealed))
	}
	envelope := buildEnvelope(sessID, nextSeq(seqCounter), sealed)
	if err := tr.Send(sess.peer, envelope); err != nil {
		log.Printf("[error] send data: %v", err)
	}
}

func sendKeepalive(tr transport, sessID uint16, current *atomic.Pointer[session], seqCounter *uint32, myObservedQualityPct int) {
	sess := current.Load()
	if sess == nil {
		return
	}
	q := myObservedQualityPct
	if q < 0 {
		q = 0
	}
	if q > 100 {
		q = 100
	}
	sealed := sess.crypt.seal([]byte{pktKeepalive, byte(q)})
	envelope := buildEnvelope(sessID, nextSeq(seqCounter), sealed)
	_ = tr.Send(sess.peer, envelope)
}

// sendPing emits a liveness/RTT probe on the currently active session and
// records the send time in stats so a matching pong can compute RTT.
func sendPing(tr transport, sessID uint16, current *atomic.Pointer[session], seqCounter *uint32, stats *linkStats, tokenCounter *uint64) {
	sess := current.Load()
	if sess == nil {
		return
	}
	token := atomic.AddUint64(tokenCounter, 1)
	body := make([]byte, 9)
	body[0] = pktPing
	binary.BigEndian.PutUint64(body[1:], token)
	sealed := sess.crypt.seal(body)
	envelope := buildEnvelope(sessID, nextSeq(seqCounter), sealed)
	if err := tr.Send(sess.peer, envelope); err == nil {
		stats.recordSend(token)
	}
}

// handleSessionPlain processes anything decrypted on an established
// session. Ping/pong/keepalive are control traffic handled entirely here;
// everything else is real data, delivered via handlePlain. activeStats and
// peerReportedQuality may be nil (the server side doesn't track outbound
// RTT or use the multi-path machinery).
func handleSessionPlain(plain []byte, sess *session, tr transport, sessID uint16, seqCounter *uint32, sink dataSink, fecDec *fecDecoder, activeStats *linkStats, peerReportedQuality *atomic.Int32) {
	if len(plain) == 0 {
		return
	}
	switch plain[0] {
	case pktPing:
		if len(plain) < 9 {
			return
		}
		pong := make([]byte, 9)
		pong[0] = pktPong
		copy(pong[1:], plain[1:9])
		sealed := sess.crypt.seal(pong)
		env := buildEnvelope(sessID, nextSeq(seqCounter), sealed)
		_ = tr.Send(sess.peer, env)
	case pktPong:
		if activeStats == nil || len(plain) < 9 {
			return
		}
		activeStats.recordPong(binary.BigEndian.Uint64(plain[1:9]))
	case pktKeepalive:
		if peerReportedQuality != nil && len(plain) >= 2 {
			peerReportedQuality.Store(int32(plain[1]))
		}
	case pktClose:
		closeSinkCurrent(sink)
	default:
		handlePlain(plain, sink, fecDec)
	}
}

// closeSinkCurrent closes just the current logical connection on a sink
// that supports it (the TCP forward sinks), leaving the listener/target
// config alone so a fresh connection can be accepted/dialed next time.
// Sinks that don't support this (TUN, UDP forward) simply no-op.
func closeSinkCurrent(sink dataSink) {
	if closer, ok := sink.(interface{ CloseCurrent() error }); ok {
		_ = closer.CloseCurrent()
	}
}

// sendClose signals the peer that this side's local TCP leg just ended, so
// they close theirs too instead of hanging on a read that will never
// return data. Only meaningful for TCP forward mode, but cheap and harmless
// to call unconditionally (a control packet, not real data) whenever a
// sink's ReadFrame errors.
func sendClose(tr transport, sessID uint16, current *atomic.Pointer[session], seqCounter *uint32) {
	sess := current.Load()
	if sess == nil {
		return
	}
	sealed := sess.crypt.seal([]byte{pktClose})
	envelope := buildEnvelope(sessID, nextSeq(seqCounter), sealed)
	_ = tr.Send(sess.peer, envelope)
}

// handlePlain applies a decrypted data-channel message: deliver data
// frames to the sink (TUN device or forward-mode UDP relay), fold FEC
// slots into the decoder, and recover a lost sibling frame when its
// parity packet allows it.
func handlePlain(plain []byte, sink dataSink, fecDec *fecDecoder) {
	if len(plain) == 0 {
		return
	}
	switch plain[0] {
	case pktKeepalive:
		return
	case pktData:
		if len(plain) < 7 {
			return
		}
		groupID := binary.BigEndian.Uint32(plain[1:5])
		index := int(plain[5])
		groupSize := int(plain[6])
		frame := plain[7:]
		if groupSize > 0 && fecDec != nil {
			fecDec.onData(groupID, index, groupSize, frame)
		}
		if err := sink.WriteFrame(frame); err != nil {
			log.Printf("[error] sink write: %v", err)
		}
	case pktParity:
		if fecDec == nil || len(plain) < 6 {
			return
		}
		groupID := binary.BigEndian.Uint32(plain[1:5])
		groupSize := int(plain[5])
		parity := plain[6:]
		if recovered := fecDec.onParity(groupID, groupSize, parity); recovered != nil {
			if err := sink.WriteFrame(recovered); err != nil {
				log.Printf("[error] sink write (fec-recovered): %v", err)
			}
		}
	}
}

// ---- client ------------------------------------------------------------

func runClient(cfg *Config, tr transport, sink dataSink, servers []string) {
	sessID := sessionIDFromPassword(cfg.Password)
	var current atomic.Pointer[session]
	var pendingMap sync.Map
	var seqCounter uint32
	var pingToken uint64
	var lastRx atomic.Int64
	lastRx.Store(time.Now().Unix())

	var fecEnc *fecEncoder
	var fecDec *fecDecoder
	if cfg.FECEnable {
		fecEnc = newFECEncoder(cfg.FECGroup)
		fecDec = newFECDecoder()
	}

	quality := newLinkQuality(cfg.LinkQuality, cfg.BanQuality)
	activeStats := newLinkStats()
	var lastRecvQualityPct atomic.Int32
	var peerReportedQuality atomic.Int32

	var candidates []*candidate
	for _, h := range servers {
		addr, err := tr.ResolveAddr(h)
		if err != nil {
			log.Fatalf("[fatal] resolve %q: %v", h, err)
		}
		candidates = append(candidates, &candidate{addr: addr, hostname: h})
	}
	var activeIdx atomic.Int32
	multiServer := len(candidates) > 1
	if multiServer {
		log.Printf("[multipath] %d candidate servers configured, auto-selecting the best path", len(candidates))
	}

	// network reader: demultiplexes hello-acks (active handshake or probes)
	// from session traffic, for the life of the process.
	go func() {
		buf := make([]byte, 65536)
		for {
			n, from, err := tr.Recv(buf)
			if err != nil {
				log.Printf("[error] recv: %v", err)
				time.Sleep(50 * time.Millisecond)
				continue
			}
			if n < 4 {
				continue
			}
			envelope := buf[:n]
			if binary.BigEndian.Uint16(envelope[0:2]) != sessID {
				continue
			}
			seq := binary.BigEndian.Uint16(envelope[2:4])
			body := envelope[4:]
			if len(body) == 0 {
				continue
			}

			if body[0] == msgHelloAck {
				completeHandshake(&pendingMap, cfg.Password, from, body)
				continue
			}

			sess := current.Load()
			if sess == nil || from.String() != sess.peer.String() {
				continue // not from the currently active server - ignore (could be a stale probe reply)
			}
			plain, err := sess.crypt.open(body)
			if err != nil {
				continue
			}
			lastRx.Store(time.Now().Unix())
			pct, banned := quality.observe(seq)
			lastRecvQualityPct.Store(int32(pct))
			if banned {
				log.Printf("[quality] link quality dropped below BAN_QUALITY (%d%%); forcing a clean reconnect", cfg.BanQuality)
				os.Exit(1)
			}
			handleSessionPlain(plain, sess, tr, sessID, &seqCounter, sink, fecDec, activeStats, &peerReportedQuality)
		}
	}()

	activateFirstReachable := func() {
		for {
			for i, c := range candidates {
				sess, rtt, err := attemptHandshake(tr, sessID, cfg.Password, c.addr, &pendingMap, &seqCounter, 2*time.Second)
				if err != nil {
					log.Printf("[handshake] %s unreachable: %v", c.hostname, err)
					continue
				}
				log.Printf("[handshake] session established with %s (%v)", c.hostname, rtt)
				current.Store(sess)
				activeIdx.Store(int32(i))
				return
			}
			log.Printf("[handshake] no configured server answered, retrying...")
		}
	}

	activateFirstReachable()

	// sink -> network
	go func() {
		// Capped well under fecSlotSize and safely below what a single
		// ICMP/UDP packet can carry - matters most for the TCP forward
		// sinks, whose Read() has no natural per-call size bound the way a
		// UDP datagram or TUN frame already does.
		buf := make([]byte, maxSinkReadSize)
		for {
			n, err := sink.ReadFrame(buf)
			if err != nil {
				log.Printf("[error] sink read: %v", err)
				sendClose(tr, sessID, &current, &seqCounter)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			frame := append([]byte(nil), buf[:n]...)
			sendData(tr, sessID, &current, &seqCounter, frame, fecEnc, nil)
		}
	}()

	pingTicker := time.NewTicker(time.Duration(cfg.PingIntervalSec) * time.Second)
	defer pingTicker.Stop()
	go func() {
		for range pingTicker.C {
			sendPing(tr, sessID, &current, &seqCounter, activeStats, &pingToken)
		}
	}()

	var healthTicker *time.Ticker
	if multiServer {
		healthTicker = time.NewTicker(time.Duration(cfg.HealthCheckSec) * time.Second)
		defer healthTicker.Stop()
	}

	keepaliveTicker := time.NewTicker(time.Duration(cfg.KeepaliveSec) * time.Second)
	defer keepaliveTicker.Stop()

	var healthC <-chan time.Time
	if healthTicker != nil {
		healthC = healthTicker.C
	}

	for {
		select {
		case <-keepaliveTicker.C:
			sendKeepalive(tr, sessID, &current, &seqCounter, int(lastRecvQualityPct.Load()))
			if fecEnc != nil {
				fecEnc.SetGroupSize(adaptiveGroupSize(cfg.FECGroup, int(peerReportedQuality.Load())))
			}
			if fecDec != nil {
				fecDec.gc(5 * time.Second)
			}
			if time.Now().Unix()-lastRx.Load() > int64(cfg.KeepaliveSec)*6 {
				log.Printf("[warn] no data from server in %ds, re-handshaking", cfg.KeepaliveSec*6)
				current.Store(nil)
				activateFirstReachable()
			}
		case <-healthC:
			runHealthChecks(cfg, tr, sessID, &pendingMap, &seqCounter, candidates, &activeIdx, &current, activeStats)
		}
	}
}

// ---- server --------------------------------------------------------------

func runServer(cfg *Config, tr transport, sink dataSink) {
	sessID := sessionIDFromPassword(cfg.Password)
	var current atomic.Pointer[session]
	var seqCounter uint32
	var lastRx atomic.Int64
	var lastRecvQualityPct atomic.Int32
	var peerReportedQuality atomic.Int32

	var fecEnc *fecEncoder
	var fecDec *fecDecoder
	if cfg.FECEnable {
		fecEnc = newFECEncoder(cfg.FECGroup)
		fecDec = newFECDecoder()
	}

	quality := newLinkQuality(cfg.LinkQuality, cfg.BanQuality)

	var bucket *tokenBucket
	if cfg.BandwidthMbps > 0 {
		bucket = newTokenBucket(cfg.BandwidthMbps)
		log.Printf("[simorgh] downstream bandwidth capped at %.2f Mbps", cfg.BandwidthMbps)
	}

	// sink -> network (no-op until a client has completed the handshake)
	go func() {
		buf := make([]byte, maxSinkReadSize)
		for {
			n, err := sink.ReadFrame(buf)
			if err != nil {
				log.Printf("[error] sink read: %v", err)
				sendClose(tr, sessID, &current, &seqCounter)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			frame := append([]byte(nil), buf[:n]...)
			sendData(tr, sessID, &current, &seqCounter, frame, fecEnc, bucket)
		}
	}()

	go func() {
		ticker := time.NewTicker(time.Duration(cfg.KeepaliveSec) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			sendKeepalive(tr, sessID, &current, &seqCounter, int(lastRecvQualityPct.Load()))
			if fecEnc != nil {
				fecEnc.SetGroupSize(adaptiveGroupSize(cfg.FECGroup, int(peerReportedQuality.Load())))
			}
			if fecDec != nil {
				fecDec.gc(5 * time.Second)
			}
			if sess := current.Load(); sess != nil && time.Now().Unix()-lastRx.Load() > int64(cfg.KeepaliveSec)*6 {
				log.Printf("[warn] no data from client in %ds - link may be down", cfg.KeepaliveSec*6)
			}
		}
	}()

	buf := make([]byte, 65536)
	for {
		n, from, err := tr.Recv(buf)
		if err != nil {
			log.Printf("[error] recv: %v", err)
			continue
		}
		if n < 4 {
			continue
		}
		envelope := buf[:n]
		if binary.BigEndian.Uint16(envelope[0:2]) != sessID {
			continue
		}
		seq := binary.BigEndian.Uint16(envelope[2:4])
		body := envelope[4:]
		if len(body) == 0 {
			continue
		}

		if body[0] == msgHello {
			clientPub, err := parseHello(cfg.Password, body[1:])
			if err != nil {
				log.Printf("[warn] handshake: %v", err)
				continue
			}
			ackMsg, priv, err := buildHelloAck(cfg.Password, clientPub.Bytes())
			if err != nil {
				log.Printf("[warn] handshake: %v", err)
				continue
			}
			shared, err := priv.ECDH(clientPub)
			if err != nil {
				log.Printf("[warn] ecdh: %v", err)
				continue
			}
			key := deriveSessionKey(shared, clientPub.Bytes(), priv.PublicKey().Bytes())
			sc, err := newSessionCrypto(key)
			if err != nil {
				log.Printf("[warn] session crypto: %v", err)
				continue
			}
			current.Store(&session{crypt: sc, peer: from})
			lastRx.Store(time.Now().Unix())
			log.Printf("[handshake] new session with %v", from)

			ackEnvelope := buildEnvelope(sessID, nextSeq(&seqCounter), ackMsg)
			if err := tr.Send(from, ackEnvelope); err != nil {
				log.Printf("[warn] send hello-ack: %v", err)
			}
			continue
		}

		sess := current.Load()
		if sess == nil {
			continue
		}
		if from.String() != sess.peer.String() {
			continue // ignore traffic not from the currently handshaken client
		}
		plain, err := sess.crypt.open(body)
		if err != nil {
			continue
		}
		lastRx.Store(time.Now().Unix())
		pct, banned := quality.observe(seq)
		lastRecvQualityPct.Store(int32(pct))
		if banned {
			log.Printf("[quality] link quality dropped below BAN_QUALITY (%d%%); forcing a clean reconnect", cfg.BanQuality)
			os.Exit(1)
		}
		handleSessionPlain(plain, sess, tr, sessID, &seqCounter, sink, fecDec, nil, &peerReportedQuality)
	}
}
