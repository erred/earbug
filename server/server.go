package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/go-logr/logr"
	"github.com/klauspost/compress/zstd"
	"github.com/zmb3/spotify/v2"
	spotifyauth "github.com/zmb3/spotify/v2/auth"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	earbugv3 "go.seankhliao.com/earbug/v3/pb/earbug/v3"
	"go.seankhliao.com/svcrunner"
	"go.seankhliao.com/svcrunner/envflag"
	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

var cookieName = "earbug_user"

type Server struct {
	spotifyID     string
	spotifySecret string
	bucket        string

	bkt    *storage.BucketHandle
	single singleflight.Group

	log   logr.Logger
	trace trace.Tracer
}

func New(hs *http.Server) *Server {
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/update", s.update)
	mux.HandleFunc("/auth/init/", s.authInit)
	mux.HandleFunc("/auth/callback", s.authCallback)
	hs.Handler = mux
	return s
}

func (s *Server) Register(c *envflag.Config) {
	c.StringVar(&s.bucket, "earbug.bucket", "", "name of storage bucket")
	c.StringVar(&s.spotifyID, "earbug.spotify-id", "", "spotify client id")
	c.StringVar(&s.spotifySecret, "earbug.spotify-secret", "", "spotify client secret")
}

func (s *Server) Init(ctx context.Context, t svcrunner.Tools) error {
	s.log = t.Log.WithName("earbug")
	s.trace = otel.Tracer("cloudbuild-gchat")

	client, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("create storge client: %w", err)
	}
	s.bkt = client.Bucket(s.bucket)
	return nil
}

func (s *Server) authInit(rw http.ResponseWriter, r *http.Request) {
	log := s.log.WithName("auth-init")
	ctx, span := s.trace.Start(r.Context(), "auth-init")
	defer span.End()

	user, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/auth/init/"), "/")
	http.SetCookie(rw, &http.Cookie{
		Name:     cookieName,
		Value:    user,
		Path:     "/",
		HttpOnly: true,
	})
	authURL := spotifyauth.New(
		spotifyauth.WithRedirectURL("https://"+r.Host+"/auth/callback"),
		spotifyauth.WithScopes(
			spotifyauth.ScopeUserReadRecentlyPlayed,
		),
		spotifyauth.WithClientID(s.spotifyID),
		spotifyauth.WithClientSecret(s.spotifySecret),
	).AuthURL(user)

	http.Redirect(rw, r, authURL, http.StatusFound)
	log.V(1).Info("redirecting auth", "user", user, "ctx", ctx, "http_request", r)
}

func (s *Server) authCallback(rw http.ResponseWriter, r *http.Request) {
	log := s.log.WithName("auth-callback")
	ctx, span := s.trace.Start(r.Context(), "auth-callback")
	defer span.End()

	msg, user, token, err := func(r *http.Request) (string, string, *oauth2.Token, error) {
		ctx, span = s.trace.Start(ctx, "get-token")
		defer span.End()

		user, err := r.Cookie(cookieName)
		if err != nil {
			return "get cookie", "", nil, err
		}

		log = log.WithValues("user", user, "ctx", ctx)
		auth := spotifyauth.New(
			spotifyauth.WithRedirectURL("https://"+r.Host+"/auth/callback"),
			spotifyauth.WithScopes(
				spotifyauth.ScopeUserReadRecentlyPlayed,
			),
			spotifyauth.WithClientID(s.spotifyID),
			spotifyauth.WithClientSecret(s.spotifySecret),
		)
		token, err := auth.Token(ctx, user.Value, r)
		if err != nil {
			return "extract token", "", nil, err
		}
		return "", user.Value, token, nil
	}(r)
	if err != nil {
		http.Error(rw, msg, http.StatusBadRequest)
		log.Error(err, msg, "ctx", ctx, "http_request", r)
		return
	}

	ctx, span = s.trace.Start(ctx, "update-auth")
	// run until we set our value
	for shared, ctr := true, 0; shared; ctr++ {
		log.V(1).Info("updating user auth", "attempt", ctr, "ctx", ctx)
		func() {
			ctx, span = s.trace.Start(ctx, "try-update-auth")
			defer span.End()

			_, err, shared = s.single.Do(user, func() (any, error) {
				ctx, span = s.trace.Start(ctx, "update-auth-singleflight")
				defer span.End()

				log.V(1).Info("getting stored user data")
				u, err := newUserData(ctx, s.bkt, user, r.Host, s.spotifyID, s.spotifySecret, token)
				if err != nil {
					return nil, fmt.Errorf("get updated user data: %w", err)
				}

				log.V(1).Info("writing to storage")
				err = u.write(ctx)
				if err != nil {
					return nil, fmt.Errorf("write updated user data: %w", err)
				}

				return nil, nil
			})
		}()
		if err != nil {
			http.Error(rw, "update stored data", http.StatusInternalServerError)
			log.Error(err, "update stored data", "ctx", ctx, "http_request", r)
			return
		}
	}

	rw.Write([]byte("user auth updated"))
	s.log.Info("user auth updated", "ctx", ctx, "http_request", r)
}

// func (s *Server)

type userReq struct {
	User string `json:"user"`
}

func (s *Server) update(rw http.ResponseWriter, r *http.Request) {
	log := s.log.WithName("update")
	ctx, span := s.trace.Start(r.Context(), "update")
	defer span.End()

	t := time.Now()

	msg, user, err := func(ctx context.Context, r *http.Request) (string, string, error) {
		_, span := s.trace.Start(ctx, "extract-user")
		defer span.End()

		if r.Method != http.MethodPost {
			return "POST only", "", errors.New("invalid method")
		}

		b, err := io.ReadAll(r.Body)
		if err != nil {
			return "read request", "", err
		}
		var ur userReq
		err = json.Unmarshal(b, &ur)
		if err != nil {
			return "unmarshal request", "", err
		} else if ur.User == "" {
			return "no user", "", errors.New("no user provided")
		}

		return "", ur.User, nil
	}(ctx, r)
	if err != nil {
		http.Error(rw, msg, http.StatusBadRequest)
		log.Error(err, msg, "ctx", ctx, "http_request", r)
		return
	}

	log = log.WithValues("user", user)

	ctx, span = s.trace.Start(ctx, "update-spotify")
	defer span.End()
	// run until we have a stats update
	var stats updateStats
	for ok, ctr := false, 0; !ok; ctr++ {
		func() {
			ctx, span := s.trace.Start(ctx, "try-update-spotify")
			defer span.End()

			log.V(1).Info("updating recently played data", "attempt", ctr, "ctx", ctx)
			var statsi any
			statsi, err, _ = s.single.Do(user, func() (any, error) {
				ctx, span := s.trace.Start(ctx, "update-spotify-singleflight")
				defer span.End()

				log.V(1).Info("getting stored user data")
				u, err := newUserData(ctx, s.bkt, user, r.Host, s.spotifyID, s.spotifySecret, nil)
				if err != nil {
					return nil, fmt.Errorf("get user data: %w", err)
				}

				stats := updateStats{
					oldTracks: len(u.data.Tracks),
					oldPlays:  len(u.data.Playbacks),
				}
				log.V(1).Info("querying spotify for recently played")
				err = u.update(ctx)
				if err != nil {
					return nil, fmt.Errorf("update %v: %w", user, err)
				}
				stats.newTracks = len(u.data.Tracks)
				stats.newPlays = len(u.data.Playbacks)

				log.V(1).Info("writing to storage")
				err = u.write(ctx)
				if err != nil {
					return nil, fmt.Errorf("write %v: %w", user, err)
				}
				return stats, nil
			})

			stats, ok = statsi.(updateStats)
		}()
		if err != nil {
			http.Error(rw, "update uer data", http.StatusInternalServerError)
			log.Error(err, "update user data", "ctx", ctx, "http_request", r)
			return
		}
	}

	rw.Write([]byte("ok"))
	log.Info("listening history updated",
		"dur", time.Since(t),
		"tracks_new", stats.newTracks-stats.oldTracks,
		"plays_new", stats.newPlays-stats.oldPlays,
		"tracks_all", stats.newTracks,
		"plays_all", stats.newPlays,
		"ctx", ctx,
		"http_request", r,
	)
}

type updateStats struct {
	oldTracks, newTracks int
	oldPlays, newPlays   int
}

type userData struct {
	obj    *storage.ObjectHandle
	data   earbugv3.Store
	client *spotify.Client
}

// reads the stored object, optionally overriding the stored token
func newUserData(ctx context.Context, bkt *storage.BucketHandle, user string, host, spotifyID, spotifySecret string, token *oauth2.Token) (*userData, error) {
	ctx, span := otel.Tracer("earbug-userdata").Start(ctx, "newUserData")
	defer span.End()

	u := &userData{
		obj: bkt.Object(user + ".pb.zstd"),
	}

	err := u.read(ctx)
	if err != nil {
		return nil, err
	}

	auth := spotifyauth.New(
		spotifyauth.WithRedirectURL("https://"+host+"/auth/callback"),
		spotifyauth.WithScopes(
			spotifyauth.ScopeUserReadRecentlyPlayed,
		),
		spotifyauth.WithClientID(spotifyID),
		spotifyauth.WithClientSecret(spotifySecret),
	)
	if token == nil {
		token = new(oauth2.Token)
		err := json.Unmarshal(u.data.Token, token)
		if err != nil {
			return nil, err
		}
	} else {
		b, err := json.Marshal(token)
		if err != nil {
			return nil, err
		}
		u.data.Token = b
	}

	authCtx := context.WithValue(ctx, oauth2.HTTPClient, &http.Client{
		Transport: otelhttp.NewTransport(nil),
	})

	httpClient := auth.Client(authCtx, token)
	httpClient.Transport = otelhttp.NewTransport(httpClient.Transport)
	u.client = spotify.New(httpClient, spotify.WithRetry(true))

	return u, nil
}

// queries spotify for the latest 50 playes tracks and updates the stored data
func (u *userData) update(ctx context.Context) error {
	ctx, span := otel.Tracer("earbug-userdata").Start(ctx, "update")
	defer span.End()

	items, err := u.client.PlayerRecentlyPlayedOpt(
		ctx,
		&spotify.RecentlyPlayedOptions{
			Limit: 50, // Max
		},
	)
	if err != nil {
		return fmt.Errorf("get recently played: %w", err)
	}

	for _, item := range items {
		ts := item.PlayedAt.Format(time.RFC3339Nano)
		if _, ok := u.data.Playbacks[ts]; !ok {
			u.data.Playbacks[ts] = &earbugv3.Playback{
				TrackId:     item.Track.ID.String(),
				TrackUri:    string(item.Track.URI),
				ContextType: item.PlaybackContext.Type,
				ContextUri:  string(item.PlaybackContext.URI),
			}
		}

		if _, ok := u.data.Tracks[item.Track.ID.String()]; !ok {
			t := &earbugv3.Track{
				Id:       item.Track.ID.String(),
				Uri:      string(item.Track.URI),
				Type:     item.Track.Type,
				Name:     item.Track.Name,
				Duration: durationpb.New(item.Track.TimeDuration()),
			}
			for _, artist := range item.Track.Artists {
				t.Artists = append(t.Artists, &earbugv3.Artist{
					Id:   artist.ID.String(),
					Uri:  string(artist.URI),
					Name: artist.Name,
				})
			}
			u.data.Tracks[item.Track.ID.String()] = t
		}
	}

	return err
}

// reads the object handle into the data field
func (u *userData) read(ctx context.Context) error {
	ctx, span := otel.Tracer("earbug-userdata").Start(ctx, "read")
	defer span.End()

	or, err := u.obj.NewReader(ctx)
	if errors.Is(err, storage.ErrBucketNotExist) {
		return errors.New("new user setup not implemented")
	} else if err != nil {
		return fmt.Errorf("read object: %w", err)
	}
	defer or.Close()

	zr, err := zstd.NewReader(or)
	if err != nil {
		return fmt.Errorf("create zstd reader: %w", err)
	}
	defer zr.Close()

	b, err := io.ReadAll(zr)
	if err != nil {
		return fmt.Errorf("read data: %w", err)
	}

	err = proto.Unmarshal(b, &u.data)
	if err != nil {
		return fmt.Errorf("unmarshal data: %w", err)
	}
	return err
}

// writes the current user data back to the object handle
func (u *userData) write(ctx context.Context) error {
	ctx, span := otel.Tracer("earbug-userdata").Start(ctx, "write")
	defer span.End()

	ow := u.obj.NewWriter(ctx)
	defer ow.Close()

	zw, err := zstd.NewWriter(ow)
	if err != nil {
		return fmt.Errorf("create zstd writer: %w", err)
	}
	defer zw.Close()

	b, err := proto.Marshal(&u.data)
	if err != nil {
		return fmt.Errorf("marshal data: %w", err)
	}

	_, err = io.Copy(zw, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("write data: %w", err)
	}
	return nil
}
