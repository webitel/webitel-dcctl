package runtime

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/webitel/webitel-dcctl/internal/config"
)

func TestServerTLS(t *testing.T) {
	p := newTestPKI(t)

	cfg, err := serverTLS(p.files)
	require.NoError(t, err)

	assert.Equal(t, tls.RequireAndVerifyClientCert, cfg.ClientAuth)
	assert.Equal(t, uint16(tls.VersionTLS13), cfg.MinVersion)
	assert.Len(t, cfg.Certificates, 1)
	assert.NotNil(t, cfg.ClientCAs)
}

func TestServerTLSErrors(t *testing.T) {
	p := newTestPKI(t)
	dir := t.TempDir()

	notPEM := filepath.Join(dir, "not.pem")
	require.NoError(t, os.WriteFile(notPEM, []byte("not a certificate"), 0o600))

	missing := filepath.Join(dir, "missing.pem")

	tests := []struct {
		name string
		mut  func(*config.TLS)
		want string
	}{
		{"missing_ca", func(c *config.TLS) { c.CA = missing }, "tls.ca"},
		{"ca_without_certificates", func(c *config.TLS) { c.CA = notPEM }, "tls.ca"},
		{"missing_cert", func(c *config.TLS) { c.Cert = missing }, "tls.cert"},
		{"bad_key", func(c *config.TLS) { c.Key = notPEM }, "tls.cert"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := p.files
			tt.mut(&files)

			_, err := serverTLS(files)
			require.ErrorContains(t, err, tt.want)
		})
	}
}
