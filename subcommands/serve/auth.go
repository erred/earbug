package serve

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/bufbuild/connect-go"
	"github.com/zmb3/spotify/v2"
	spotifyauth "github.com/zmb3/spotify/v2/auth"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	earbugv4 "go.seankhliao.com/proto/earbug/v4"
)

func (s *Server) Authorize(ctx context.Context, r *connect.Request[earbugv4.AuthorizeRequest]) (*connect.Response[earbugv4.AuthorizeResponse], error) {
	clientID, clientSecret := func() (clientID, clientSecret string) {
		s.storemu.Lock()
		defer s.storemu.Unlock()
		clientID = r.Msg.ClientId
		if clientID == "" {
			clientID = s.store.Auth.ClientId
		} else {
			if s.store.Auth == nil {
				s.store.Auth = &earbugv4.Auth{}
			}
			s.store.Auth.ClientId = clientID
		}
		clientSecret = r.Msg.ClientSecret
		if clientSecret == "" {
			clientSecret = s.store.Auth.ClientSecret
		} else {
			s.store.Auth.ClientSecret = clientSecret
		}
		return
	}()

	as := NewAuthState(clientID, clientSecret, s.authURL)
	s.authState.Store(as)

	return &connect.Response[earbugv4.AuthorizeResponse]{
		Msg: &earbugv4.AuthorizeResponse{
			AuthUrl: as.auth.AuthURL(as.state),
		},
	}, nil
}

func (s *Server) hAuthCallback(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	as := s.authState.Load()
	token, err := as.auth.Token(ctx, as.state, r)
	if err != nil {
		s.o.httpError(ctx, "get token from request", err, rw, http.StatusBadRequest)
		return
	}

	httpClient := as.auth.Client(ctx, token)
	httpClient.Transport = otelhttp.NewTransport(httpClient.Transport)
	spotClient := spotify.New(httpClient)

	tokenMarshaled, err := json.Marshal(token)
	if err != nil {
		s.o.httpError(ctx, "marshal token", err, rw, http.StatusBadRequest)
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
	auth  *spotifyauth.Authenticator
}

func NewAuthState(clientID, clientSecret, redirectURL string) *AuthState {
	buf := make([]byte, 256)
	rand.Read(buf)
	return &AuthState{
		state: base64.StdEncoding.EncodeToString(buf),
		auth: spotifyauth.New(
			spotifyauth.WithClientID(clientID),
			spotifyauth.WithClientSecret(clientSecret),
			spotifyauth.WithScopes(
				spotifyauth.ScopeUserReadRecentlyPlayed,
			),
			spotifyauth.WithRedirectURL(redirectURL),
		),
	}
}
