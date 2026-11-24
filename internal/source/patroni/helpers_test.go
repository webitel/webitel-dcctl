package patroni_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/source/patroni"
)

const testPassword = "s3cr3t-p4ss"

// pki is a self-signed certificate for 127.0.0.1 that is its own CA.
type pki struct {
	cert   tls.Certificate
	caFile string
}

func newPKI(t *testing.T) *pki {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "patroni.test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))

	return &pki{
		cert:   tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		caFile: caFile,
	}
}

// serve starts an HTTPS server with the certificate of p and returns its host:port.
func (p *pki) serve(t *testing.T, h http.Handler) string {
	t.Helper()

	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{p.cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return strings.TrimPrefix(srv.URL, "https://")
}

// deadAddr returns host:port nothing listens on.
func deadAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := l.Addr().String()
	require.NoError(t, l.Close())

	return addr
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)

	return b
}

func serveJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func newClient(t *testing.T, p *pki, opts patroni.Options) *patroni.Client {
	t.Helper()

	opts.CAFile = p.caFile
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = 2 * time.Second
	}

	c, err := patroni.New(opts)
	require.NoError(t, err)

	return c
}

type fakeResolver struct {
	addrs   []string
	err     error
	gotDC   string
	gotName string
	gotTag  string
}

func (f *fakeResolver) HealthyAddrs(_ context.Context, dc, service, tag string) ([]string, error) {
	f.gotDC, f.gotName, f.gotTag = dc, service, tag

	return f.addrs, f.err
}

func ptr[T any](v T) *T { return &v }
