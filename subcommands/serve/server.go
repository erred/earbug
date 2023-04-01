package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
	"github.com/zmb3/spotify/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.seankhliao.com/earbug/v4/observability"
	earbugv4 "go.seankhliao.com/proto/earbug/v4"
	"go.seankhliao.com/proto/earbug/v4/earbugv4connect"
	"gocloud.dev/blob"
	"golang.org/x/exp/slog"
	"golang.org/x/oauth2"
	"google.golang.org/protobuf/proto"
	"tailscale.com/tsnet"

	_ "gocloud.dev/blob/fileblob"
	_ "gocloud.dev/blob/gcsblob"
	oauthspotify "golang.org/x/oauth2/spotify"
)

type Server struct {
	o O

	http *http.Server
	ts   *tsnet.Server
	spot *spotify.Client

	storemu sync.Mutex
	store   earbugv4.Store

	authURL   string
	authState atomic.Pointer[AuthState]

	earbugv4connect.UnimplementedEarbugServiceHandler
}

func New(ctx context.Context, c *Cmd) *Server {
	o := O{observability.New(c.o11yConf)}
	mux := http.NewServeMux()
	s := &Server{
		o: o,
		http: &http.Server{
			Addr:    c.address,
			Handler: mux,
			// ErrorLog: ,
		},
		ts: &tsnet.Server{
			Hostname:  "earbug",
			Dir:       c.dir,
			Ephemeral: true,
			Logf: func(f string, args ...any) {
				ctx := context.Background()
				o.L.LogAttrs(ctx, slog.LevelDebug, "tailscale server",
					slog.Group("tailscale", slog.String("server", fmt.Sprintf(f, args...))),
				)
			},
		},
	}

	p, h := earbugv4connect.NewEarbugServiceHandler(s)
	mux.Handle(p, otelhttp.NewHandler(h, "earbugv4connect"))
	mux.Handle("/auth/callback", otelhttp.NewHandler(http.HandlerFunc(s.hAuthCallback), "authCallback"))
	mux.HandleFunc("/-/ready", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte("ok")) })

	s.initData(ctx, c.bucket, c.key)

	return s
}

func (s *Server) initData(ctx context.Context, bucket, key string) error {
	if bucket != "" && key != "" {
		bkt, err := blob.OpenBucket(ctx, bucket)
		if err != nil {
			return s.o.markErr(ctx, "open bucket", err)
		}
		defer bkt.Close()
		or, err := bkt.NewReader(ctx, key, nil)
		if err != nil {
			return s.o.markErr(ctx, "open object", err)
		}
		defer or.Close()
		zr, err := zstd.NewReader(or)
		if err != nil {
			return s.o.markErr(ctx, "new zstd reader", err)
		}
		defer or.Close()
		b, err := io.ReadAll(zr)
		if err != nil {
			return s.o.markErr(ctx, "read object", err)
		}
		err = proto.Unmarshal(b, &s.store)
		if err != nil {
			return s.o.markErr(ctx, "unmarshal store", err)
		}

		rawToken := s.store.Token // old value
		if s.store.Auth != nil && len(s.store.Auth.Token) > 0 {
			rawToken = s.store.Auth.Token // new value
		} else {
			s.o.L.LogAttrs(ctx, slog.LevelWarn, "falling back to deprecated token field")
		}

		var token oauth2.Token
		err = json.Unmarshal(rawToken, &token)
		if err != nil {
			return s.o.markErr(ctx, "unmarshal oauth token", err)
		}

		httpClient := (&oauth2.Config{Endpoint: oauthspotify.Endpoint}).Client(ctx, &token)
		httpClient.Transport = otelhttp.NewTransport(httpClient.Transport)
		s.spot = spotify.New(httpClient)

		return nil
	}

	s.o.L.LogAttrs(ctx, slog.LevelWarn, "no initial data provided")
	s.spot = spotify.New(http.DefaultClient)
	s.store = earbugv4.Store{
		Playbacks: make(map[string]*earbugv4.Playback),
		Tracks:    make(map[string]*earbugv4.Track),
		Auth:      &earbugv4.Auth{},
	}
	return nil
}

func (s *Server) Run(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		s.o.L.LogAttrs(ctx, slog.LevelInfo, "shutting down")
		s.http.Shutdown(context.Background())
	}()

	var lis net.Listener
	var err error
	if s.http.Addr == "funnel" {
		s.o.L.LogAttrs(ctx, slog.LevelInfo, "starting funnel")
		lis, err = s.ts.ListenFunnel("tcp", ":443")
		if err != nil {
			return s.o.markErr(ctx, "listen tailscale funnel", err)
		}
		for _, dom := range s.ts.CertDomains() {
			if strings.HasSuffix(dom, ".ts.net") {
				s.authURL = (&url.URL{Scheme: "https", Host: dom, Path: "/auth/callback"}).String()
				s.o.L.LogAttrs(ctx, slog.LevelDebug, "setting auth callback url",
					slog.String("url", s.authURL),
				)
				break
			}
		}
	} else {
		lis, err = net.Listen("tcp", s.http.Addr)
		if err != nil {
			return s.o.markErr(ctx, "listen locally", err)
		}
	}
	err = s.http.Serve(lis)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return s.o.markErr(ctx, "unexpected server shutdown", err)
	}
	return nil
}

type O struct {
	*observability.O
}

func (o *O) markErr(ctx context.Context, msg string, err error) error {
	o.L.LogAttrs(ctx, slog.LevelError, msg, slog.String("error", err.Error()))
	return fmt.Errorf("%s: %w", msg, err)
}

func (o *O) httpError(ctx context.Context, msg string, err error, rw http.ResponseWriter, code int) {
	err = o.markErr(ctx, msg, err)
	http.Error(rw, err.Error(), code)
}
