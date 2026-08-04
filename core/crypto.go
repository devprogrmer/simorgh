package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
)

// packet types - first byte of the AEAD plaintext, once a session exists.
const (
	pktData      byte = 0x01
	pktKeepalive byte = 0x02 // [pktKeepalive][observedQualityPct byte] - reports how clean *incoming* traffic looked to the sender, so the peer can adapt its FEC
	pktParity    byte = 0x03
	pktPing      byte = 0x04 // [pktPing][8-byte token] - liveness/RTT probe on an already-established session
	pktPong      byte = 0x05 // [pktPong][8-byte token]  - echo of the same token
	pktClose     byte = 0x06 // no body - "my local TCP leg just closed, close yours too" (forward mode, FORWARD_PROTO=tcp)
)

// sessionCrypto wraps an AES-256-GCM AEAD keyed from a per-session key
// produced by the X25519 handshake (see handshake.go). A brand new key is
// negotiated on every handshake, giving forward secrecy: compromising one
// session's key (or the long-term password) does not expose past sessions'
// traffic, since the key was never transmitted, only its ECDH output was.
type sessionCrypto struct {
	aead cipher.AEAD
}

func newSessionCrypto(sessionKey []byte) (*sessionCrypto, error) {
	if len(sessionKey) != 32 {
		return nil, fmt.Errorf("session key must be 32 bytes, got %d", len(sessionKey))
	}
	block, err := aes.NewCipher(sessionKey)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &sessionCrypto{aead: aead}, nil
}

func (s *sessionCrypto) seal(plaintext []byte) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	_, _ = rand.Read(nonce)
	out := make([]byte, 0, len(nonce)+len(plaintext)+s.aead.Overhead())
	out = append(out, nonce...)
	out = s.aead.Seal(out, nonce, plaintext, nil)
	return out
}

func (s *sessionCrypto) open(data []byte) ([]byte, error) {
	ns := s.aead.NonceSize()
	if len(data) < ns {
		return nil, fmt.Errorf("short packet")
	}
	nonce, ct := data[:ns], data[ns:]
	return s.aead.Open(nil, nonce, ct, nil)
}

// sessionIDFromKey derives the 16-bit filter id used at the envelope level,
// scoped to the *password* (not the session key) so both peers agree on it
// before any handshake has happened.
func sessionIDFromPassword(password string) uint16 {
	h := sha256.Sum256(append([]byte("simorgh-session-id|"), password...))
	return uint16(h[0])<<8 | uint16(h[1])
}
