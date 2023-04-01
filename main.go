package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/subcommands"
	"go.seankhliao.com/earbug/v4/subcommands/authorize"
	"go.seankhliao.com/earbug/v4/subcommands/export"
	"go.seankhliao.com/earbug/v4/subcommands/report"
	"go.seankhliao.com/earbug/v4/subcommands/serve"
	"go.seankhliao.com/earbug/v4/subcommands/update"
)

func main() {
	name := "earbug"
	fset := flag.NewFlagSet(name, flag.ExitOnError)
	cmdr := subcommands.NewCommander(fset, name)
	cmdr.Register(&serve.Cmd{}, "server")

	cmdr.Register(&authorize.Cmd{}, "client")
	cmdr.Register(&export.Cmd{}, "client")
	cmdr.Register(&report.Cmd{}, "client")
	cmdr.Register(&update.Cmd{}, "client")

	cmdr.Register(cmdr.HelpCommand(), "other")

	fset.Parse(os.Args[1:])

	ctx := context.Background()
	ctx, _ = signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	os.Exit(int(cmdr.Execute(ctx)))
}
