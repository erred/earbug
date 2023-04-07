package serve

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/bufbuild/connect-go"
	"github.com/zmb3/spotify/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	earbugv4 "go.seankhliao.com/proto/earbug/v4"
	"golang.org/x/oauth2"
	oauthspotify "golang.org/x/oauth2/spotify"
)

func (s *Server) Authorize(ctx context.Context, r *connect.Request[earbugv4.AuthorizeRequest]) (*connect.Response[earbugv4.AuthorizeResponse], error) {
	_, span := s.o.T.Start(ctx, "Authorize")
	defer span.End()

	clientID, clientSecret := func() (clientID, clientSecret string) {
		s.storemu.Lock()
		defer s.storemu.Unlock()
		clientID = r.Msg.ClientId
		if clientID == "" && (s.store.Auth != nil && s.store.Auth.ClientId != "") {
			clientID = s.store.Auth.ClientId
		} else {
			if s.store.Auth == nil {
				s.store.Auth = &earbugv4.Auth{}
			}
			s.store.Auth.ClientId = clientID
		}
		clientSecret = r.Msg.ClientSecret
		if clientSecret == "" && (s.store.Auth != nil && s.store.Auth.ClientSecret != "") {
			clientSecret = s.store.Auth.ClientSecret
		} else {
			s.store.Auth.ClientSecret = clientSecret
		}
		return
	}()
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("missing client id/secret")
	}

	as := NewAuthState(clientID, clientSecret, s.authURL)
	s.authState.Store(as)

	return &connect.Response[earbugv4.AuthorizeResponse]{
		Msg: &earbugv4.AuthorizeResponse{
			AuthUrl: as.conf.AuthCodeURL(as.state),
		},
	}, nil
}

func (s *Server) hAuthCallback(rw http.ResponseWriter, r *http.Request) {
	ctx, span := s.o.T.Start(r.Context(), "hAuthCallback")
	defer span.End()

	as := s.authState.Load()
	token, err := as.conf.Exchange(ctx, r.FormValue("code"))
	if err != nil {
		s.o.HTTPErr(ctx, "get token from request", err, rw, http.StatusBadRequest)
		return
	}

	httpClient := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)
	httpClient = as.conf.Client(ctx, token)
	spotClient := spotify.New(httpClient)

	tokenMarshaled, err := json.Marshal(token)
	if err != nil {
		s.o.HTTPErr(ctx, "marshal token", err, rw, http.StatusBadRequest)
		return
	}

	func() {
		s.storemu.Lock()
		defer s.storemu.Unlock()
		s.store.Auth.Token = tokenMarshaled
		s.spot = spotClient
	}()

	rw.Write([]byte("success"))
}

type AuthState struct {
	state string
	conf  *oauth2.Config
}

func NewAuthState(clientID, clientSecret, redirectURL string) *AuthState {
	buf := make([]byte, 256)
	rand.Read(buf)
	return &AuthState{
		state: base64.StdEncoding.EncodeToString(buf),
		conf: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint: oauth2.Endpoint{
				AuthURL:   oauthspotify.Endpoint.AuthURL,
				TokenURL:  oauthspotify.Endpoint.TokenURL,
				AuthStyle: oauth2.AuthStyleInHeader,
			},
			RedirectURL: redirectURL,
			Scopes:      []string{"user-read-recently-played"},
		},
	}
}
