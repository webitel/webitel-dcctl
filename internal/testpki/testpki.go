// Package testpki issues throwaway certificates for tests: a CA and leaf
// certificates with chosen SAN IPs, in memory or as PEM files.
package testpki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/config"
)

// CA is a certificate authority valid for an hour around now.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	// Pool trusts only Cert.
	Pool *x509.CertPool
}

// New returns a new CA named name.
func New(t *testing.T, name string) *CA {
	t.Helper()

	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}

	cert, key := sign(t, tmpl, nil, nil)

	pool := x509.NewCertPool()
	pool.AddCert(cert)

	return &CA{Cert: cert, Key: key, Pool: pool}
}

// Leaf issues a certificate for server and client auth with the SAN IPs ips.
func (ca *CA) Leaf(t *testing.T, ips ...string) tls.Certificate {
	t.Helper()

	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: "node"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}

	for _, ip := range ips {
		parsed := net.ParseIP(ip)
		require.NotNil(t, parsed, "SAN %q is not an IP", ip)
		tmpl.IPAddresses = append(tmpl.IPAddresses, parsed)
	}

	cert, key := sign(t, tmpl, ca.Cert, ca.Key)

	return tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key, Leaf: cert}
}

// Files writes the CA and a new leaf with the SAN IPs ips into a temporary
// directory, the way the pki role lays them out.
func (ca *CA) Files(t *testing.T, ips ...string) config.TLS {
	t.Helper()

	leaf := ca.Leaf(t, ips...)
	dir := t.TempDir()

	files := config.TLS{
		CA:   filepath.Join(dir, "ca.pem"),
		Cert: filepath.Join(dir, "node.pem"),
		Key:  filepath.Join(dir, "node-key.pem"),
	}

	key, err := x509.MarshalPKCS8PrivateKey(leaf.PrivateKey)
	require.NoError(t, err)

	writePEM(t, files.CA, "CERTIFICATE", ca.Cert.Raw)
	writePEM(t, files.Cert, "CERTIFICATE", leaf.Leaf.Raw)
	writePEM(t, files.Key, "PRIVATE KEY", key)

	return files
}

// sign self-signs tmpl when parent is nil.
func sign(t *testing.T, tmpl, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	if parent == nil {
		parent, parentKey = tmpl, key
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	return cert, key
}

func serial(t *testing.T) *big.Int {
	t.Helper()

	n, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)

	return n
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()

	data := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	require.NoError(t, os.WriteFile(path, data, 0o600))
}
