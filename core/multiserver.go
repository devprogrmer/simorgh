package main

import (
	"encoding/binary"
	"log"
	"net"
	"sync/atomic"
	"time"
)

// customerState is one customer's fully isolated slice of server state:
// its own session crypto, FEC, quality tracking, and relay sink to the
// real target (WireGuard/OpenVPN/etc). Nothing here is ever shared across
// customers - that's the whole point of multi-client isolation.
type customerState struct {
	cust *customer
	sink dataSink

	current    atomic.Pointer[session]
	seqCounter uint32
	lastRx     atomic.Int64

	lastRecvQualityPct  atomic.Int32
	peerReportedQuality atomic.Int32

	quality *linkQuality
	fecEnc  *fecEncoder
	fecDec  *fecDecoder

	bytesIn  atomic.Int64
	bytesOut atomic.Int64
}

func newCustomerState(cust *customer, cfg *Config) (*customerState, error) {
	var sink dataSink
	var err error
	if cfg.ForwardProto == "tcp" {
		sink, err = newTCPServerForwardSink(cfg.TargetHost, cfg.TargetPort)
	} else {
		sink, err = newUDPServerForwardSink(cfg.TargetHost, cfg.TargetPort)
	}
	if err != nil {
		return nil, err
	}

	cs := &customerState{
		cust:    cust,
		sink:    sink,
		quality: newLinkQuality(cfg.LinkQuality, cfg.BanQuality),
	}
	if cfg.FECEnable {
		cs.fecEnc = newFECEncoder(cfg.FECGroup)
		cs.fecDec = newFECDecoder()
	}
	return cs, nil
}

// runServerMulti is runServer's multi-tenant counterpart: one process
// serving many customers, each demultiplexed by the sessID their own
// password deterministically produces. Currently forward-mode only (see
// the MODE=forward requirement enforced in loadConfig) - tun mode's shared
// TUN device makes per-customer isolation a materially harder problem,
// deliberately deferred rather than done half-correctly.
func runServerMulti(cfg *Config, tr transport, customers []*customer) {
	log.Printf("[simorgh] multi-client mode: serving %d customer(s) from %s", len(customers), cfg.CustomersFile)

	states := make(map[uint16]*customerState, len(customers))
	for _, cust := range customers {
		cs, err := newCustomerState(cust, cfg)
		if err != nil {
			log.Fatalf("[fatal] customer %q: %v", cust.Name, err)
		}
		states[cust.sessID] = cs
		log.Printf("[simorgh] customer %q ready (relaying to %s:%d)", cust.Name, cfg.TargetHost, cfg.TargetPort)

		go runCustomerSinkReader(cfg, tr, cs)
	}

	go runCustomerTicker(cfg, tr, states)

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
		sessID := binary.BigEndian.Uint16(envelope[0:2])
		cs, ok := states[sessID]
		if !ok {
			continue // not one of our customers - ignore (stray traffic, or wrong password)
		}
		seq := binary.BigEndian.Uint16(envelope[2:4])
		body := envelope[4:]
		if len(body) == 0 {
			continue
		}

		if body[0] == msgHello {
			handleCustomerHello(tr, cs, from, body)
			continue
		}

		sess := cs.current.Load()
		if sess == nil || from.String() != sess.peer.String() {
			continue
		}
		plain, err := sess.crypt.open(body)
		if err != nil {
			continue
		}
		cs.bytesIn.Add(int64(len(body)))
		cs.lastRx.Store(time.Now().Unix())
		pct, banned := cs.quality.observe(seq)
		cs.lastRecvQualityPct.Store(int32(pct))
		if banned {
			log.Printf("[quality] customer %q dropped below BAN_QUALITY (%d%%); dropping their session so they reconnect cleanly", cs.cust.Name, cfg.BanQuality)
			cs.current.Store(nil)
			continue // drop only this customer's session, never the whole process
		}
		handleSessionPlain(plain, sess, tr, sessID, &cs.seqCounter, cs.sink, cs.fecDec, nil, &cs.peerReportedQuality)
	}
}

func handleCustomerHello(tr transport, cs *customerState, from net.Addr, body []byte) {
	clientPub, err := parseHello(cs.cust.Password, body[1:])
	if err != nil {
		log.Printf("[warn] customer %q: handshake: %v", cs.cust.Name, err)
		return
	}
	ackMsg, priv, err := buildHelloAck(cs.cust.Password, clientPub.Bytes())
	if err != nil {
		log.Printf("[warn] customer %q: handshake: %v", cs.cust.Name, err)
		return
	}
	shared, err := priv.ECDH(clientPub)
	if err != nil {
		log.Printf("[warn] customer %q: ecdh: %v", cs.cust.Name, err)
		return
	}
	key := deriveSessionKey(shared, clientPub.Bytes(), priv.PublicKey().Bytes())
	sc, err := newSessionCrypto(key)
	if err != nil {
		log.Printf("[warn] customer %q: session crypto: %v", cs.cust.Name, err)
		return
	}

	log.Printf("[handshake] new session for customer %q (%v)", cs.cust.Name, from)
	cs.current.Store(&session{crypt: sc, peer: from})
	cs.lastRx.Store(time.Now().Unix())

	ackEnvelope := buildEnvelope(cs.cust.sessID, nextSeq(&cs.seqCounter), ackMsg)
	if err := tr.Send(from, ackEnvelope); err != nil {
		log.Printf("[warn] customer %q: send hello-ack: %v", cs.cust.Name, err)
	}
}

// runCustomerSinkReader is this customer's "sink -> network" direction:
// reads whatever comes back from their real target (WireGuard/OpenVPN/etc)
// and tunnels it back to them, exactly like the single-client sendData path
// but scoped to one customer's session/FEC/bucket state. The bandwidth cap
// (if any) is this customer's own bandwidth_mbps from CUSTOMERS_FILE, not
// the global BANDWIDTH_LIMIT env var - each customer can have a different
// cap, or none.
func runCustomerSinkReader(cfg *Config, tr transport, cs *customerState) {
	var bucket *tokenBucket
	if cs.cust.BandwidthMbps > 0 {
		bucket = newTokenBucket(cs.cust.BandwidthMbps)
		log.Printf("[simorgh] customer %q capped at %.2f Mbps", cs.cust.Name, cs.cust.BandwidthMbps)
	}
	buf := make([]byte, maxSinkReadSize)
	for {
		n, err := cs.sink.ReadFrame(buf)
		if err != nil {
			log.Printf("[error] customer %q: sink read: %v", cs.cust.Name, err)
			sendClose(tr, cs.cust.sessID, &cs.current, &cs.seqCounter)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		frame := append([]byte(nil), buf[:n]...)
		cs.bytesOut.Add(int64(len(frame)))
		sendData(tr, cs.cust.sessID, &cs.current, &cs.seqCounter, frame, cs.fecEnc, bucket)
	}
}

// runCustomerTicker drives keepalives, adaptive FEC, and stale-session
// detection for every customer from one shared ticker rather than spawning
// per-customer timer goroutines.
func runCustomerTicker(cfg *Config, tr transport, states map[uint16]*customerState) {
	ticker := time.NewTicker(time.Duration(cfg.KeepaliveSec) * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		for _, cs := range states {
			sendKeepalive(tr, cs.cust.sessID, &cs.current, &cs.seqCounter, int(cs.lastRecvQualityPct.Load()))
			if cs.fecEnc != nil {
				cs.fecEnc.SetGroupSize(adaptiveGroupSize(cfg.FECGroup, int(cs.peerReportedQuality.Load())))
			}
			if cs.fecDec != nil {
				cs.fecDec.gc(5 * time.Second)
			}
			if sess := cs.current.Load(); sess != nil && time.Now().Unix()-cs.lastRx.Load() > int64(cfg.KeepaliveSec)*6 {
				log.Printf("[warn] customer %q: no data in %ds - link may be down", cs.cust.Name, cfg.KeepaliveSec*6)
			}
		}
	}
}
