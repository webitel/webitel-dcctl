package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/webitel/webitel-go-kit/infra/discovery"
	otelsdk "github.com/webitel/webitel-go-kit/infra/otel/sdk"
	"github.com/webitel/webitel-go-kit/pkg/logging"

	"github.com/webitel/webitel-dcctl/internal/buildinfo"

	_ "github.com/webitel/webitel-go-kit/infra/otel/sdk/log/otlp"
	_ "github.com/webitel/webitel-go-kit/infra/otel/sdk/log/stdout"
	_ "github.com/webitel/webitel-go-kit/infra/otel/sdk/metric/otlp"
	_ "github.com/webitel/webitel-go-kit/infra/otel/sdk/metric/prometheus"
	_ "github.com/webitel/webitel-go-kit/infra/otel/sdk/metric/stdout"
	_ "github.com/webitel/webitel-go-kit/infra/otel/sdk/trace/otlp"
	_ "github.com/webitel/webitel-go-kit/infra/otel/sdk/trace/stdout"
)

// telemetryFlushTimeout bounds the export of what is left on exit.
const telemetryFlushTimeout = 5 * time.Second

// setupTelemetry configures the OpenTelemetry SDK from the OTEL_* variables.
// Records before that go to stderr through the bootstrap logger.
func setupTelemetry(ctx context.Context, command string) (shutdown func() error, err error) {
	logging.Bootstrap().InfoCtx(ctx, "starting",
		logging.Str("command", command),
		logging.Str("version", buildinfo.Version),
	)

	res, err := serviceResource(command)
	if err != nil {
		return nil, err
	}

	sdkShutdown, err := otelsdk.Configure(ctx, otelsdk.WithResource(res))

	shutdown = func() error {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), telemetryFlushTimeout)
		defer cancel()

		if err := sdkShutdown(sctx); err != nil {
			return fmt.Errorf("shut down OpenTelemetry: %w", err)
		}

		return nil
	}

	if err != nil {
		return nil, errors.Join(fmt.Errorf("configure OpenTelemetry: %w", err), shutdown())
	}

	return shutdown, nil
}

// serviceResource names the process of command as the observer of its metrics. The
// instance id of serve is the one it registers in Consul with; the arbiter has one of
// its own, as both may run on one host.
func serviceResource(command string) (*resource.Resource, error) {
	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("host name: %w", err)
	}

	instance := buildinfo.ServiceName
	if command != "serve" {
		instance += "-" + command
	}

	return resource.NewSchemaless(
		semconv.ServiceName(buildinfo.ServiceName),
		semconv.ServiceVersion(buildinfo.Version),
		semconv.ServiceNamespace("webitel"),
		semconv.ServiceInstanceID(discovery.GenerateInstanceID(instance)),
		semconv.HostName(host),
	), nil
}
