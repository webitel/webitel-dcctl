package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

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

const (
	serviceName = "webitel-dcctl"
	// telemetryFlushTimeout bounds the export of what is left on exit.
	telemetryFlushTimeout = 5 * time.Second
)

// setupTelemetry configures the OpenTelemetry SDK from the OTEL_* variables.
// Records before that go to stderr through the bootstrap logger.
func setupTelemetry(ctx context.Context, command string) (shutdown func() error, err error) {
	logging.Bootstrap().InfoCtx(ctx, "starting",
		logging.Str("command", command),
		logging.Str("version", buildinfo.Version),
	)

	sdkShutdown, err := otelsdk.Configure(ctx, otelsdk.WithResource(resource.NewSchemaless(
		semconv.ServiceName(serviceName),
		semconv.ServiceVersion(buildinfo.Version),
		semconv.ServiceNamespace("webitel"),
	)))

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
