package export

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/google/subcommands"
	"go.seankhliao.com/earbug/v4/client"
	earbugv4 "go.seankhliao.com/proto/earbug/v4"
	"go.seankhliao.com/proto/earbug/v4/earbugv4connect"
	"go.seankhliao.com/svcrunner/v2/observability"
)

type Cmd struct {
	o observability.Config
	c client.Config

	bucket string
	key    string

	freq time.Duration
}

func (c *Cmd) Name() string     { return `export` }
func (c *Cmd) Synopsis() string { return `export all data to a server location or return locally` }
func (c *Cmd) Usage() string {
	return `export [options...]
export -bucket file://... -key out.pb.ztsd
export -bucket local -key out.pb.ztsd

Export to a server location (bucket) or return it to the local client.

Flags:
`
}

func (c *Cmd) SetFlags(f *flag.FlagSet) {
	c.o.SetFlags(f)
	c.c.SetFlags(f)

	f.StringVar(&c.bucket, "bucket", "", "target bucket")
	f.StringVar(&c.key, "key", "", "target key")
	f.Func("frequency", "run in loop on this interval", func(s string) error {
		d, err := time.ParseDuration(s)
		c.freq = d
		return err
	})
}

func (c *Cmd) Execute(ctx context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	o := observability.New(c.o)
	e := client.New(c.c)

	err := c.export(ctx, o, e)
	if err != nil {
		return subcommands.ExitFailure
	}
	if c.freq != 0 {
		err = c.exportLoop(ctx, o, e)
		if err != nil {
			return subcommands.ExitFailure
		}
	}
	return subcommands.ExitSuccess
}

func (c *Cmd) export(ctx context.Context, o *observability.O, e earbugv4connect.EarbugServiceClient) error {
	ctx, span := o.T.Start(ctx, "export")
	defer span.End()

	er := &earbugv4.ExportRequest{}
	if c.bucket != "local" {
		er.Bucket = c.bucket
		er.Key = c.key
	}
	res, err := e.Export(ctx, &connect.Request[earbugv4.ExportRequest]{
		Msg: er,
	})
	if err != nil {
		o.L.LogAttrs(ctx, slog.LevelError, "send export request", slog.String("error", err.Error()))
		return err
	}

	if c.bucket == "local" {
		err = os.WriteFile(c.key, res.Msg.Content, 0o644)
		if err != nil {
			o.L.LogAttrs(ctx, slog.LevelError, "write local output", slog.String("error", err.Error()))
			return err
		}
	}
	return nil
}

func (c *Cmd) exportLoop(ctx context.Context, o *observability.O, e earbugv4connect.EarbugServiceClient) error {
	for {
		select {
		case <-time.NewTicker(c.freq).C:
			c.export(ctx, o, e)
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}
