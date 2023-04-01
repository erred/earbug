package observability

import (
	"flag"
	"io"
	"os"

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
	L *slog.Logger
}

func New(c Config) *O {
	logOptions := &slog.HandlerOptions{
		Level: c.LogLevel,
	}
	out := c.LogOutput
	if out == nil {
		out = os.Stdout
	}
	logHandler := logOptions.NewJSONHandler(out)

	return &O{
		L: slog.New(logHandler),
	}
}
