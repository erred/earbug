package authorize

import (
	"context"
	"flag"
	"fmt"

	"github.com/bufbuild/connect-go"
	"github.com/google/subcommands"
	"go.seankhliao.com/earbug/v4/client"
	"go.seankhliao.com/earbug/v4/observability"
	earbugv4 "go.seankhliao.com/proto/earbug/v4"
	"golang.org/x/exp/slog"
)

type Cmd struct {
	o observability.Config
	c client.Config

	clientID     string
	clientSecret string
}

func (c *Cmd) Name() string     { return `authorize` }
func (c *Cmd) Synopsis() string { return `update spotify authorization data in the server` }
func (c *Cmd) Usage() string {
	return `authorize [options...]

(re)authorize the server with new oauth client id/secret (optional) and oauth grant / token.

Flags:
`
}

func (c *Cmd) SetFlags(f *flag.FlagSet) {
	c.o.SetFlags(f)
	c.c.SetFlags(f)

	f.StringVar(&c.clientID, "client.id", "", "spotify app oauth client id")
	f.StringVar(&c.clientSecret, "client.secret", "", "spotify app oauth client secret")
}

func (c *Cmd) Execute(ctx context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	o := observability.New(c.o)
	e := client.New(c.c)

	ctx, span := o.T.Start(ctx, "auth")
	defer span.End()

	res, err := e.Authorize(ctx, &connect.Request[earbugv4.AuthorizeRequest]{
		Msg: &earbugv4.AuthorizeRequest{
			ClientId:     c.clientID,
			ClientSecret: c.clientSecret,
		},
	})
	if err != nil {
		o.L.LogAttrs(ctx, slog.LevelError, "send authorize request", slog.String("error", err.Error()))
		return subcommands.ExitFailure
	}

	fmt.Printf("please visit the url to continue\n\n\t%s\n\n", res.Msg.AuthUrl)
	return subcommands.ExitSuccess
}
