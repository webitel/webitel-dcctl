package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"time"

	"github.com/urfave/cli/v2"
	"go.yaml.in/yaml/v3"
	"google.golang.org/grpc"
	grpccreds "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/webitel/webitel-dcctl/internal/config"
)

// defaultServer is where listen.http is when no config says otherwise.
const defaultServer = "127.0.0.1:10022"

// serverAddress returns the address of the API: --server, else listen.http of
// the serve config, else the default. A config the operator cannot read or
// that has no listen.http is not an error: the CLI also runs on a host that
// has no dcctl config.
func serverAddress(c *cli.Context, o Options) (string, error) {
	if s := c.String(flagServer); s != "" {
		return s, nil
	}

	path := o.ConfigPath
	if path == "" {
		path = config.DefaultServePath
	}

	data, err := os.ReadFile(path) // #nosec G304 -- the path is the documented config location.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
			return defaultServer, nil
		}

		return "", fmt.Errorf("read %s: %w", path, err)
	}

	var cfg struct {
		Listen struct {
			HTTP string `yaml:"http"`
		} `yaml:"listen"`
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}

	if cfg.Listen.HTTP == "" {
		return defaultServer, nil
	}

	host, port, err := net.SplitHostPort(cfg.Listen.HTTP)
	if err != nil {
		return "", fmt.Errorf("listen.http in %s: %w", path, err)
	}

	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}

	return net.JoinHostPort(host, port), nil
}

// loopbackAddrs returns the addresses to dial for addr, which must be on this
// host: its host is an IP literal that is loopback, or a name every address of
// which is loopback. The addresses have IP literals, so the connection goes
// where the check looked; all of them are returned, as the server may listen
// on one family only.
func loopbackAddrs(ctx context.Context, addr string) ([]string, bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, false
	}

	if ip := net.ParseIP(host); ip != nil {
		return []string{addr}, ip.IsLoopback()
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, false
	}

	addrs := make([]string, len(ips))

	for i, ip := range ips {
		if !ip.IP.IsLoopback() {
			return nil, false
		}

		addrs[i] = net.JoinHostPort(ip.IP.String(), port)
	}

	return addrs, true
}

// dialEach returns a dialer that tries the addresses in turn.
func dialEach(addrs []string) func(ctx context.Context, _ string) (net.Conn, error) {
	return func(ctx context.Context, _ string) (net.Conn, error) {
		var errs []error

		for _, a := range addrs {
			conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", a)
			if err == nil {
				return conn, nil
			}

			errs = append(errs, err)
		}

		return nil, errors.Join(errs...)
	}
}

// transport returns the transport credentials for addr and, for a plaintext
// connection, the addresses to dial. Without --tls or --tls-ca the connection
// is h2c, which the server offers on loopback: the password must not cross a
// network in the clear, so any other address needs TLS. With TLS the name stays
// the target, so the server name and the authority are the host that was given.
func transport(ctx context.Context, c *cli.Context, addr string) (grpccreds.TransportCredentials, []string, error) {
	caFile := c.String(flagTLSCA)

	if !c.Bool(flagTLS) && caFile == "" {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return nil, nil, usageErrorf("--%s %q: %v", flagServer, addr, err)
		}

		addrs, ok := loopbackAddrs(ctx, addr)
		if !ok {
			return nil, nil, usageErrorf("the server %s is not on this host: add --%s or --%s, "+
				"the password is not sent without TLS", addr, flagTLS, flagTLSCA)
		}

		return insecure.NewCredentials(), addrs, nil
	}

	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if caFile != "" {
		pem, err := os.ReadFile(caFile) // #nosec G304 -- the operator names their own CA file.
		if err != nil {
			return nil, nil, fmt.Errorf("--%s: %w", flagTLSCA, err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, nil, fmt.Errorf("--%s %s: no certificate in the file", flagTLSCA, caFile)
		}

		cfg.RootCAs = pool
	}

	return grpccreds.NewTLS(cfg), nil, nil
}

// dial returns the connection to the API for the command. It connects on the
// first call.
func dial(c *cli.Context, o Options, creds credentials) (*grpc.ClientConn, error) {
	addr, err := serverAddress(c, o)
	if err != nil {
		return nil, err
	}

	tc, addrs, err := transport(c.Context, c, addr)
	if err != nil {
		return nil, err
	}

	opts := []grpc.DialOption{grpc.WithTransportCredentials(tc), grpc.WithPerRPCCredentials(creds)}

	if addrs != nil {
		// The addresses were checked to be loopback: the dialer goes to them, not
		// to the name.
		addr = "passthrough:///" + addr

		opts = append(opts, grpc.WithContextDialer(dialEach(addrs)))
	}

	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}

	return conn, nil
}
