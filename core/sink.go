package main

import "os"

// dataSink is whatever local endpoint frames are read from / written to.
// In "tun" mode that's a TUN device carrying raw IP frames; in "forward"
// mode it's a local UDP relay carrying another VPN's (WireGuard/OpenVPN)
// already-encrypted datagrams. Everything else - handshake, session
// crypto, FEC, link quality, keepalive - is identical either way.
type dataSink interface {
	ReadFrame(buf []byte) (int, error)
	WriteFrame(frame []byte) error
	Close() error
}

type tunSink struct{ f *os.File }

func (t *tunSink) ReadFrame(buf []byte) (int, error) { return t.f.Read(buf) }
func (t *tunSink) WriteFrame(frame []byte) error     { _, err := t.f.Write(frame); return err }
func (t *tunSink) Close() error                      { return t.f.Close() }
