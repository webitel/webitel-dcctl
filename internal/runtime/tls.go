package runtime

import (
	"crypto/tls"

	"github.com/webitel/webitel-dcctl/internal/config"
)

// serverTLS presents the node certificate and requires a client certificate
// signed by the CA.
func serverTLS(c config.TLS) (*tls.Config, error) {
	cert, pool, err := c.Load()
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}
