package client

import (
	"flag"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.seankhliao.com/proto/earbug/v4/earbugv4connect"
)

type Config struct {
	address string
}

func (c *Config) SetFlags(f *flag.FlagSet) {
	f.StringVar(&c.address, "server.address", "https://earbug.badger-altered.ts.net", "address of server to connect to")
}

func New(c Config) earbugv4connect.EarbugServiceClient {
	httpClient := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}
	return earbugv4connect.NewEarbugServiceClient(httpClient, c.address)
}
