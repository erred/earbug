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
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/go-logr/logr"
	"github.com/klauspost/compress/zstd"
	"github.com/zmb3/spotify/v2"
	spotifyauth "github.com/zmb3/spotify/v2/auth"
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
	bkt           *storage.BucketHandle
	single        singleflight.Group
	cacheMu       sync.Mutex
	cache         map[string]*userData
	log           logr.Logger
	authOnce      sync.Once
	auth          *spotifyauth.Authenticator
}

func New(hs *http.Server) *Server {
	s := &Server{
		cache: make(map[string]*userData),
	}
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
	client, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("create storge client: %w", err)
	}
	s.bkt = client.Bucket(s.bucket)
	return nil
}

func (s *Server) authInit(rw http.ResponseWriter, r *http.Request) {
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
	s.log.V(1).Info("redirecting auth", "handler", "authInit", "user", user)
}

func (s *Server) authCallback(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := s.log.WithValues("handler", "authCallback")

	user, err := r.Cookie(cookieName)
	if err != nil {
		http.Error(rw, "get cookie", http.StatusBadRequest)
		log.Error(err, "get cookie", "cookie", cookieName)
		return
	}

	log = log.WithValues("user", user)
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
		http.Error(rw, "get token", http.StatusNotFound)
		log.Error(err, "get token for user auth")
		return
	}

	// run until we set our value
	for shared, ctr := true, 0; shared; ctr++ {
		log.V(1).Info("updating user auth", "attempt", ctr)
		_, err, shared = s.single.Do(user.Value, func() (any, error) {
			log.V(1).Info("getting stored user data")
			u, err := newUserData(ctx, s.bkt, user.Value, r.Host, s.spotifyID, s.spotifySecret, token)
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
		if err != nil {
			http.Error(rw, "update stored data", http.StatusInternalServerError)
			log.Error(err, "update stored data")
		}
	}

	rw.Write([]byte("user auth updated"))
	s.log.Info("user auth updated")
}

// func (s *Server)

type userReq struct {
	User string `json:"user"`
}

func (s *Server) update(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := s.log.WithValues("handler", "update")
	t := time.Now()
	if r.Method != http.MethodPost {
		http.Error(rw, "POST only", http.StatusMethodNotAllowed)
		log.V(1).Info("invalid method", "method", r.Method)
		return
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(rw, "read body", http.StatusBadRequest)
		log.Error(err, "read request body")
		return
	}
	var user userReq
	err = json.Unmarshal(b, &user)
	if err == nil && user.User == "" {
		err = errors.New("no user provided")
	}
	if err != nil {
		http.Error(rw, "unmarshal body", http.StatusBadRequest)
		log.Error(err, "unmarshal body")
		return
	}

	log = log.WithValues("user", user.User)

	// run until we have a stats update
	var stats updateStats
	for ok, ctr := false, 0; !ok; ctr++ {
		log.V(1).Info("updating recently played data", "attempt", ctr)
		statsi, err, _ := s.single.Do(user.User, func() (any, error) {
			log.V(1).Info("getting stored user data")
			u, err := newUserData(ctx, s.bkt, user.User, r.Host, s.spotifyID, s.spotifySecret, nil)
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
		if err != nil {
			http.Error(rw, "update uer data", http.StatusInternalServerError)
			log.Error(err, "update user data")
			return
		}

		stats, ok = statsi.(updateStats)
	}

	rw.Write([]byte("ok"))
	log.Info("listening history updated",
		"user", user.User,
		"dur", time.Since(t),
		"tracks_new", stats.newTracks-stats.oldTracks,
		"plays_new", stats.newPlays-stats.oldPlays,
		"tracks_all", stats.newTracks,
		"plays_all", stats.newPlays,
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
	httpClient := auth.Client(context.Background(), token)
	u.client = spotify.New(httpClient, spotify.WithRetry(true))

	return u, nil
}

// queries spotify for the latest 50 playes tracks and updates the stored data
func (u *userData) update(ctx context.Context) error {
	items, err := u.client.PlayerRecentlyPlayedOpt(
		context.Background(),
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
