package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"go.seankhliao.com/svcrunner/v2/observability"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func main() {
	conf := &Config{}

	// flags
	fset := flag.NewFlagSet("earbug", flag.ExitOnError)
	conf.SetFlags(fset)
	fset.Parse(os.Args[1:])

	// observability
	o := observability.New(conf.o)

	// context
	ctx := context.Background()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err := run(ctx, o, conf)
	if err != nil {
		o.Err(ctx, "exit", err)
	}
}

func run(ctx context.Context, o *observability.O, conf *Config) error {
	mux := http.NewServeMux()
	h2svr := &http2.Server{}
	svr := &http.Server{
		Handler:  h2c.NewHandler(mux, h2svr),
		ErrorLog: slog.NewLogLogger(o.H, slog.LevelWarn),
	}

	app, err := New(ctx, o, conf)
	if err != nil {
		return o.Err(ctx, "app setup", err)
	}
	app.Register(mux)
	defer app.export(context.Background())

	addr := conf.address
	o.L.LogAttrs(ctx, slog.LevelInfo, "starting listener", slog.String("address", addr))
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return o.Err(ctx, "failed to listen", err, slog.String("address", addr))
	}

	go func() {
		<-ctx.Done()
		o.L.LogAttrs(ctx, slog.LevelInfo, "starting shutdown", slog.String("reason", context.Cause(ctx).Error()))
		err := lis.Close()
		if err != nil {
			o.Err(ctx, "failed to close listener", err, slog.String("address", addr))
		}
		err = svr.Shutdown(context.Background())
		if err != nil {
			o.Err(ctx, "failed to close server", err, slog.String("address", addr))
		}
	}()

	err = svr.Serve(lis)
	if err != nil {
		return o.Err(ctx, "failed to serve", err)
	}
	return nil
}
