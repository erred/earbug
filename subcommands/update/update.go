package update

import (
	"context"
	"flag"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/google/subcommands"
	"go.seankhliao.com/earbug/v4/client"
	earbugv4 "go.seankhliao.com/proto/earbug/v4"
	"go.seankhliao.com/proto/earbug/v4/earbugv4connect"
	"go.seankhliao.com/svcrunner/v2/observability"
	"golang.org/x/exp/slog"
)

type Cmd struct {
	o observability.Config
	c client.Config

	freq time.Duration
}

func (c *Cmd) Name() string     { return `update` }
func (c *Cmd) Synopsis() string { return `update recent data from spotify` }
func (c *Cmd) Usage() string {
	return `update [options...]

Trigger a scrape on the server for recent data

Flags:
`
}

func (c *Cmd) SetFlags(f *flag.FlagSet) {
	c.o.SetFlags(f)
	c.c.SetFlags(f)

	f.Func("frequency", "run in loop on this interval", func(s string) error {
		d, err := time.ParseDuration(s)
		c.freq = d
		return err
	})
}

func (c *Cmd) Execute(ctx context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	o := observability.New(c.o)
	e := client.New(c.c)

	err := c.update(ctx, o, e)
	if err != nil {
		return subcommands.ExitFailure
	}

	if c.freq != 0 {
		err = c.updateLoop(ctx, o, e)
		if err != nil {
			return subcommands.ExitFailure
		}
	}

	return subcommands.ExitSuccess
}

func (c *Cmd) update(ctx context.Context, o *observability.O, e earbugv4connect.EarbugServiceClient) error {
	ctx, span := o.T.Start(ctx, "update")
	defer span.End()

	_, err := e.UpdateRecentlyPlayed(ctx, &connect.Request[earbugv4.UpdateRecentlyPlayedRequest]{})
	if err != nil {
		o.L.LogAttrs(ctx, slog.LevelError, "send update request", slog.String("error", err.Error()))
		return err
	}
	return nil
}

func (c *Cmd) updateLoop(ctx context.Context, o *observability.O, e earbugv4connect.EarbugServiceClient) error {
	for {
		select {
		case <-time.NewTicker(c.freq).C:
			c.update(ctx, o, e)
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}
