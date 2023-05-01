package serve

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
	"github.com/zmb3/spotify/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	earbugv4 "go.seankhliao.com/proto/earbug/v4"
	"go.seankhliao.com/proto/earbug/v4/earbugv4connect"
	"go.seankhliao.com/svcrunner/v2/observability"
	"go.seankhliao.com/svcrunner/v2/tshttp"
	"go.seankhliao.com/webstyle"
	"gocloud.dev/blob"
	_ "gocloud.dev/blob/fileblob"
	_ "gocloud.dev/blob/gcsblob"
	"golang.org/x/oauth2"
	"google.golang.org/protobuf/proto"
)

type Server struct {
	o *observability.O

	svr  *tshttp.Server
	spot *spotify.Client

	storemu sync.Mutex
	store   earbugv4.Store

	authURL   string
	authState atomic.Pointer[AuthState]

	render webstyle.Renderer

	earbugv4connect.UnimplementedEarbugServiceHandler
}

func New(ctx context.Context, c *Cmd) *Server {
	svr := tshttp.New(ctx, c.tshttp)
	s := &Server{
		o:   svr.O,
		svr: svr,

		render: webstyle.NewRenderer(webstyle.TemplateCompact),
	}

	p, h := earbugv4connect.NewEarbugServiceHandler(s)
	svr.Mux.Handle(p, otelhttp.NewHandler(h, "earbugv4connect"))
	svr.Mux.Handle("/auth/callback", otelhttp.NewHandler(http.HandlerFunc(s.hAuthCallback), "authCallback"))
	svr.Mux.HandleFunc("/-/ready", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte("ok")) })
	svr.Mux.HandleFunc("/", s.handleIndex)
	svr.Mux.HandleFunc("/artists", s.handleArtists)
	svr.Mux.HandleFunc("/playbacks", s.handlePlaybacks)
	svr.Mux.HandleFunc("/tracks", s.handleTracks)

	s.initData(ctx, c.bucket, c.key)

	return s
}

func (s *Server) initData(ctx context.Context, bucket, key string) error {
	ctx, span := s.o.T.Start(ctx, "initData")
	defer span.End()

	if bucket != "" && key != "" {
		bkt, err := blob.OpenBucket(ctx, bucket)
		if err != nil {
			return s.o.Err(ctx, "open bucket", err)
		}
		defer bkt.Close()
		or, err := bkt.NewReader(ctx, key, nil)
		if err != nil {
			return s.o.Err(ctx, "open object", err)
		}
		defer or.Close()
		zr, err := zstd.NewReader(or)
		if err != nil {
			return s.o.Err(ctx, "new zstd reader", err)
		}
		defer or.Close()
		b, err := io.ReadAll(zr)
		if err != nil {
			return s.o.Err(ctx, "read object", err)
		}
		err = proto.Unmarshal(b, &s.store)
		if err != nil {
			return s.o.Err(ctx, "unmarshal store", err)
		}

		var token oauth2.Token
		if s.store.Auth != nil && len(s.store.Auth.Token) > 0 {
			rawToken := s.store.Auth.Token // new value
			err = json.Unmarshal(rawToken, &token)
			if err != nil {
				return s.o.Err(ctx, "unmarshal oauth token", err)
			}
		} else {
			s.o.L.LogAttrs(ctx, slog.LevelWarn, "no auth token found")
		}

		httpClient := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
		ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)
		as := NewAuthState(s.store.Auth.ClientId, s.store.Auth.ClientSecret, "")
		httpClient = as.conf.Client(ctx, &token)
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
	return s.svr.Run(ctx)
}
