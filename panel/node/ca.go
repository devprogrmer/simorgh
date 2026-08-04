package node

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

// Certificate handling for the node control plane.
//
// The master runs its own small CA. It signs one certificate for itself (the
// client) and one per node (the server), and each side is configured to trust
// nothing but that CA. This is not defence in depth on top of some other
// authentication -- it IS the authentication. A node applies configuration as
// root, so an unauthenticated node API is a remote root shell, and there is no
// password or token anywhere in this path to fall back on.

const (
	caValidity   = 10 * 365 * 24 * time.Hour
	leafValidity = 2 * 365 * 24 * time.Hour
)

// GenerateCA creates the master's certificate authority, returning PEM blocks
// suitable for storing in the panel database.
//
// P-256 rather than RSA: the keys and handshakes are smaller, every Go client
// and server here supports it natively, and there is no external party whose
// legacy expectations have to be met.
func GenerateCA() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Simorgh Node CA", Organization: []string{"Simorgh"}},
		NotBefore:             time.Now().Add(-time.Hour), // tolerate modest clock skew between panel and node
		NotAfter:              time.Now().Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true, // signs leaves only; it must never mint another CA
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = encodeKey(key)
	if err != nil {
		return nil, nil, err
	}
	return encodeCert(der), keyPEM, nil
}

// IssueNodeCert signs a server certificate for one node, valid for the host the
// master will dial it at.
//
// The host goes into the SAN list -- as an IP SAN when it parses as an address,
// a DNS SAN otherwise. Go's verifier ignores the Common Name entirely, so a host
// that only appeared there would fail verification with an error that reads like
// a trust problem rather than a missing SAN, which is a genuinely hard afternoon.
func IssueNodeCert(caCertPEM, caKeyPEM []byte, host string) (certPEM, keyPEM []byte, err error) {
	return issueLeaf(caCertPEM, caKeyPEM, "node:"+host, host,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
}

// IssueMasterCert signs the client certificate the master presents to nodes.
// It needs no SAN: nodes authenticate it by CA, not by address, which is what
// lets a panel move hosts without reissuing every node's trust.
func IssueMasterCert(caCertPEM, caKeyPEM []byte) (certPEM, keyPEM []byte, err error) {
	return issueLeaf(caCertPEM, caKeyPEM, "simorgh-master", "",
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
}

func issueLeaf(caCertPEM, caKeyPEM []byte, commonName, host string, usage []x509.ExtKeyUsage) (certPEM, keyPEM []byte, err error) {
	caCert, caKey, err := parseCA(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName, Organization: []string{"Simorgh"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  usage,
	}
	if host != "" {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = []net.IP{ip}
		} else {
			tmpl.DNSNames = []string{host}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	certPEM = encodeCert(der)
	keyPEM, err = encodeKey(key)
	return certPEM, keyPEM, err
}

// NodeTLSConfig is what a node serves with: its own certificate, and a demand
// that every client prove it was signed by the master's CA.
func NodeTLSConfig(caCertPEM, certPEM, keyPEM []byte) (*tls.Config, error) {
	leaf, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("node certificate does not load: %w", err)
	}
	pool, err := CertPool(caCertPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{leaf},
		// RequireAndVerifyClientCert is the entire security boundary of the node
		// API. Its neighbours are dangerously similar-looking:
		// RequireAnyClientCert takes any certificate at all including a
		// self-signed one, and VerifyClientCertIfGiven admits a client that
		// offers none. Either would leave a root-level API open to anyone who can
		// reach the port.
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  pool,
		MinVersion: tls.VersionTLS13,
	}, nil
}

// MasterTLSConfig is what the master dials with: its client certificate, and a
// demand that the node prove itself too.
//
// Verifying the node matters as much as the node verifying the master: an
// address that has been taken over would otherwise be handed every inbound's
// configuration, credentials included.
func MasterTLSConfig(caCertPEM, certPEM, keyPEM []byte, serverName string) (*tls.Config, error) {
	leaf, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("master certificate does not load: %w", err)
	}
	pool, err := CertPool(caCertPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{leaf},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// CertPool builds a trust pool from a PEM CA certificate.
//
// It reports an error on input it could not use, rather than returning an empty
// pool. An empty pool trusts nothing, so every connection fails with a
// handshake error that says nothing about the real cause -- a truncated column
// from a partial restore -- and the operator debugs the network instead.
func CertPool(caCertPEM []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caCertPEM) {
		return nil, errors.New("node: CA certificate is not usable PEM")
	}
	return pool, nil
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, errors.New("node: CA certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("node: CA certificate: %w", err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, nil, errors.New("node: CA key is not PEM")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("node: CA key: %w", err)
	}
	return cert, key, nil
}

// parseLeaf returns the parsed leaf of a loaded key pair. tls.X509KeyPair leaves
// Leaf nil, so anything wanting to inspect the certificate has to parse it back.
func parseLeaf(pair tls.Certificate) (*x509.Certificate, error) {
	if len(pair.Certificate) == 0 {
		return nil, errors.New("node: certificate pair carries no certificate")
	}
	return x509.ParseCertificate(pair.Certificate[0])
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func encodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}
