package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// Load reads the node certificate with its key and a pool with the CA.
func (t TLS) Load() (tls.Certificate, *x509.CertPool, error) {
	cert, err := tls.LoadX509KeyPair(t.Cert, t.Key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("tls.cert, tls.key: %w", err)
	}

	caPEM, err := os.ReadFile(t.CA)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("tls.ca: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, fmt.Errorf("tls.ca: no PEM certificates in %s", t.CA)
	}

	return cert, pool, nil
}
