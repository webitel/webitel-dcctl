package runtime

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

// testPKI is a CA with a node certificate for 127.0.0.1, written to files the
// way the pki role lays them out.
type testPKI struct {
	files config.TLS
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey
	pool  *x509.CertPool
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()

	ca, caKey := newCA(t, "test ca")
	dir := t.TempDir()

	p := testPKI{
		files: config.TLS{
			CA:   filepath.Join(dir, "ca.pem"),
			Cert: filepath.Join(dir, "node.pem"),
			Key:  filepath.Join(dir, "node-key.pem"),
		},
		ca:    ca,
		caKey: caKey,
		pool:  x509.NewCertPool(),
	}
	p.pool.AddCert(ca)

	node, nodeKey := newLeaf(t, ca, caKey, "node")
	writePEM(t, p.files.CA, "CERTIFICATE", ca.Raw)
	writePEM(t, p.files.Cert, "CERTIFICATE", node.Raw)
	writePEM(t, p.files.Key, "PRIVATE KEY", marshalKey(t, nodeKey))

	return p
}

// clientTLS returns a client config that trusts the CA and presents a
// certificate signed by it, by a foreign CA, or none.
func (p testPKI) clientTLS(t *testing.T, cert clientCert) *tls.Config {
	t.Helper()

	cfg := &tls.Config{RootCAs: p.pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13}

	switch cert {
	case certNone:
	case certSigned:
		leaf, key := newLeaf(t, p.ca, p.caKey, "client")
		cfg.Certificates = []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}}
	case certForeign:
		ca, caKey := newCA(t, "foreign ca")
		leaf, key := newLeaf(t, ca, caKey, "client")
		cfg.Certificates = []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}}
	}

	return cfg
}

type clientCert int

const (
	certNone clientCert = iota
	certSigned
	certForeign
)

func newCA(t *testing.T, name string) (*x509.Certificate, *ecdsa.PrivateKey) {
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

	return sign(t, tmpl, nil, nil)
}

func newLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, name string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()

	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}

	return sign(t, tmpl, ca, caKey)
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

func marshalKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()

	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	return der
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()

	data := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	require.NoError(t, os.WriteFile(path, data, 0o600))
}
