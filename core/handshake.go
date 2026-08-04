package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
)

// Handshake messages. These are the only two packet types ever sent
// unencrypted (there is no session key yet); authenticity comes from an
// HMAC keyed with a value derived from the shared tunnel password, not from
// the session key we are still negotiating.
const (
	msgHello    byte = 0xF0 // client -> server: clientPub(32) || hmac(32)
	msgHelloAck byte = 0xF1 // server -> client: serverPub(32) || hmac(32)
)

const x25519KeyLen = 32
const hmacLen = 32

func handshakeMACKey(password string) []byte {
	h := sha256.Sum256(append([]byte("simorgh-handshake|"), password...))
	return h[:]
}

func hmacOf(key []byte, parts ...[]byte) []byte {
	mac := hmac.New(sha256.New, key)
	for _, p := range parts {
		mac.Write(p)
	}
	return mac.Sum(nil)
}

// buildHello constructs the client's first handshake message and returns
// the ephemeral keypair it generated (the private half is needed later to
// compute the shared secret once the server answers).
func buildHello(password string) (msg []byte, priv *ecdh.PrivateKey, err error) {
	curve := ecdh.X25519()
	priv, err = curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ephemeral key: %w", err)
	}
	pub := priv.PublicKey().Bytes()
	mac := hmacOf(handshakeMACKey(password), pub)

	msg = make([]byte, 1+x25519KeyLen+hmacLen)
	msg[0] = msgHello
	copy(msg[1:], pub)
	copy(msg[1+x25519KeyLen:], mac)
	return msg, priv, nil
}

// parseHello validates an incoming Hello and, on success, returns the
// client's ephemeral public key.
func parseHello(password string, body []byte) (clientPub *ecdh.PublicKey, err error) {
	if len(body) != x25519KeyLen+hmacLen {
		return nil, fmt.Errorf("bad hello length")
	}
	pubBytes := body[:x25519KeyLen]
	mac := body[x25519KeyLen:]
	expected := hmacOf(handshakeMACKey(password), pubBytes)
	if !hmac.Equal(mac, expected) {
		return nil, fmt.Errorf("hello: bad hmac (wrong password or spoofed peer)")
	}
	curve := ecdh.X25519()
	return curve.NewPublicKey(pubBytes)
}

// buildHelloAck is the server's response: its own ephemeral public key,
// HMAC-bound to *both* public keys so the exchange can't be replayed against
// a different client key.
func buildHelloAck(password string, clientPubBytes []byte) (msg []byte, priv *ecdh.PrivateKey, err error) {
	curve := ecdh.X25519()
	priv, err = curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ephemeral key: %w", err)
	}
	pub := priv.PublicKey().Bytes()
	mac := hmacOf(handshakeMACKey(password), pub, clientPubBytes)

	msg = make([]byte, 1+x25519KeyLen+hmacLen)
	msg[0] = msgHelloAck
	copy(msg[1:], pub)
	copy(msg[1+x25519KeyLen:], mac)
	return msg, priv, nil
}

// parseHelloAck validates the server's response against the client's own
// public key bytes, and returns the server's ephemeral public key.
func parseHelloAck(password string, clientPubBytes, body []byte) (serverPub *ecdh.PublicKey, err error) {
	if len(body) != x25519KeyLen+hmacLen {
		return nil, fmt.Errorf("bad hello-ack length")
	}
	pubBytes := body[:x25519KeyLen]
	mac := body[x25519KeyLen:]
	expected := hmacOf(handshakeMACKey(password), pubBytes, clientPubBytes)
	if !hmac.Equal(mac, expected) {
		return nil, fmt.Errorf("hello-ack: bad hmac (wrong password or spoofed peer)")
	}
	curve := ecdh.X25519()
	return curve.NewPublicKey(pubBytes)
}

// deriveSessionKey combines the ECDH shared secret with both parties'
// ephemeral public keys (transcript binding) into the 32-byte AES-256 key.
func deriveSessionKey(shared, clientPub, serverPub []byte) []byte {
	h := sha256.New()
	h.Write([]byte("simorgh-session-key|"))
	h.Write(shared)
	// canonical order so both sides compute the same digest regardless of role
	if bytes.Compare(clientPub, serverPub) < 0 {
		h.Write(clientPub)
		h.Write(serverPub)
	} else {
		h.Write(serverPub)
		h.Write(clientPub)
	}
	return h.Sum(nil)
}
