package report

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/google/subcommands"
	"go.seankhliao.com/earbug/v4/client"
	earbugv4 "go.seankhliao.com/proto/earbug/v4"
	"go.seankhliao.com/svcrunner/v2/observability"
	"golang.org/x/exp/slog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Cmd struct {
	o observability.Config
	c client.Config

	since *timestamppb.Timestamp
}

func (c *Cmd) Name() string     { return `report` }
func (c *Cmd) Synopsis() string { return `report a summary of recent data` }
func (c *Cmd) Usage() string {
	return `report [options...]

report to a server location (bucket) or return it to the local client.

Flags:
`
}

func (c *Cmd) SetFlags(f *flag.FlagSet) {
	c.o.SetFlags(f)
	c.c.SetFlags(f)

	f.Func("since", "report data since", func(s string) error {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return err
		}
		c.since = timestamppb.New(t)
		return nil
	})
}

func (c *Cmd) Execute(ctx context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	o := observability.New(c.o)
	e := client.New(c.c)

	ctx, span := o.T.Start(ctx, "report")
	defer span.End()

	res, err := e.ReportPlayed(ctx, &connect.Request[earbugv4.ReportPlayedRequest]{
		Msg: &earbugv4.ReportPlayedRequest{
			Since: c.since,
		},
	})
	if err != nil {
		o.L.LogAttrs(ctx, slog.LevelError, "get recently played", slog.String("error", err.Error()))
		return subcommands.ExitFailure
	}
	for _, play := range res.Msg.Playbacks {
		fmt.Printf("%s\t%s\n", play.Track.Name, play.Artists[0].Name)
	}

	return subcommands.ExitSuccess
}
