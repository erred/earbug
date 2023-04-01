package observability

import (
	"context"
	"flag"
	"io"
	"os"
	"runtime/debug"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/exp/slog"
)

type Config struct {
	LogOutput io.Writer
	LogLevel  slog.Level
}

func (c *Config) SetFlags(f *flag.FlagSet) {
	f.TextVar(&c.LogLevel, "log.level", slog.LevelInfo, "log level: debug|info|warn|error")
}

type O struct {
	N string
	L *slog.Logger
	H slog.Handler
	T trace.Tracer
	M metric.Meter
}

func New(c Config) *O {
	o := &O{}

	bi, _ := debug.ReadBuildInfo()
	fullname := bi.Main.Path
	o.N = "earbug"

	defer func() {
		// always set instrumentation, even if they may be noops
		o.T = otel.Tracer(fullname)
		o.M = otel.Meter(fullname)
	}()

	logOptions := &slog.HandlerOptions{
		Level: c.LogLevel,
	}
	out := c.LogOutput
	if out == nil {
		out = os.Stdout
	}
	o.H = logOptions.NewJSONHandler(out)
	o.L = slog.New(o.H)

	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		ctx := context.Background()

		te, err := otlptracegrpc.New(ctx)
		if err != nil {
			o.L.LogAttrs(ctx, slog.LevelError, "create trace exporter", slog.String("error", err.Error()))
			return o
		}
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(te),
		)
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.Baggage{},
			propagation.TraceContext{},
		))

		me, err := otlpmetricgrpc.New(ctx)
		if err != nil {
		}
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(
				sdkmetric.NewPeriodicReader(me),
			),
		)
		otel.SetMeterProvider(mp)
	}

	return o
}
