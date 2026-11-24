package runtime

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/webitel/webitel-dcctl/internal/config"
)

// serverTLS presents the node certificate and requires a client certificate
// signed by the CA.
func serverTLS(c config.TLS) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(c.Cert, c.Key)
	if err != nil {
		return nil, fmt.Errorf("tls.cert, tls.key: %w", err)
	}

	caPEM, err := os.ReadFile(c.CA)
	if err != nil {
		return nil, fmt.Errorf("tls.ca: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("tls.ca: no PEM certificates in %s", c.CA)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}
