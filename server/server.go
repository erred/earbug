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
	s.log.V(1).Info("redirecting auth", "handler", "authInit", "user", user)
	http.Redirect(rw, r, authURL, http.StatusFound)
}

func (s *Server) authCallback(rw http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := s.log.WithValues("handler", "authCallback")
	user, err := r.Cookie(cookieName)
	if err != nil {
		http.Error(rw, "get cookie", http.StatusBadRequest)
		s.log.Error(err, "get cookie", "cookie", cookieName)
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
		log.Error(err, "get token")
		return
	}

	s.cacheMu.Lock()
	data := s.cache[user.Value]
	s.cacheMu.Unlock()
	if data == nil {
		data = &userData{}
		data.initOnce.Do(func() {
			err = data.init(ctx, s.bkt, user.Value, r.Host, s.spotifyID, s.spotifySecret)
		})
	}
	if err != nil {
		http.Error(rw, "init data", http.StatusInternalServerError)
		log.Error(err, "init user data")
		return
	}

	b, err := json.Marshal(token)
	if err != nil {
		http.Error(rw, "marshal token", http.StatusInternalServerError)
		s.log.Error(err, "marshal token")
		return
	}

	data.data.Token = b
	ts := oauth2.StaticTokenSource(token)
	httpClient := oauth2.NewClient(context.Background(), ts)
	data.client = spotify.New(httpClient, spotify.WithRetry(true))

	s.cacheMu.Lock()
	s.cache[user.Value] = data
	s.cacheMu.Unlock()

	err = data.write(ctx)
	if err != nil {
		http.Error(rw, "write data", http.StatusInternalServerError)
		s.log.Error(err, "write data")
		return
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

	statsi, err, _ := s.single.Do(user.User, func() (any, error) {
		return s.updateUser(ctx, user.User, r.Host)
	})
	if err != nil {
		http.Error(rw, "update uer data", http.StatusInternalServerError)
		log.Error(err, "update user data")
		return
	}
	rw.WriteHeader(http.StatusOK)
	stats := statsi.(updateStats)
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

func (s *Server) updateUser(ctx context.Context, user, host string) (updateStats, error) {
	log := s.log.WithValues("user", user)
	log.V(1).Info("checking for cached user", "user", user)
	data := func() *userData {
		s.cacheMu.Lock()
		defer s.cacheMu.Unlock()
		data, ok := s.cache[user]
		if !ok {
			log.V(1).Info("creating new user")
			data = &userData{}
			s.cache[user] = data
		}
		return data
	}()

	var err error
	data.initOnce.Do(func() {
		log.V(1).Info("running user data init")
		err = data.init(ctx, s.bkt, user, host, s.spotifyID, s.spotifySecret)
	})
	if err != nil {
		return updateStats{}, fmt.Errorf("init %v: %w", user, err)
	}

	log.V(1).Info("starting update", "user", user)
	stats := updateStats{
		oldTracks: len(data.data.Tracks),
		oldPlays:  len(data.data.Playbacks),
	}
	err = data.update(ctx)
	if err != nil {
		return updateStats{}, fmt.Errorf("update %v: %w", user, err)
	}
	stats.newTracks = len(data.data.Tracks)
	stats.newPlays = len(data.data.Playbacks)

	log.V(1).Info("writing to storage")
	err = data.write(ctx)
	if err != nil {
		return updateStats{}, fmt.Errorf("write %v: %w", user, err)
	}

	return stats, nil
}

type userData struct {
	initOnce sync.Once
	obj      *storage.ObjectHandle
	data     earbugv3.Store
	client   *spotify.Client
}

func (u *userData) init(ctx context.Context, bkt *storage.BucketHandle, user, host, spotifyID, spotifySecret string) error {
	u.obj = bkt.Object(user + ".pb.zstd")
	err := u.read(ctx)
	if err != nil {
		return err
	}

	var token oauth2.Token
	err = json.Unmarshal(u.data.Token, &token)
	if err != nil {
		return fmt.Errorf("unmarshal oauth2 token: %w", err)
	}

	auth := spotifyauth.New(
		spotifyauth.WithRedirectURL("https://"+host+"/auth/callback"),
		spotifyauth.WithScopes(
			spotifyauth.ScopeUserReadRecentlyPlayed,
		),
		spotifyauth.WithClientID(spotifyID),
		spotifyauth.WithClientSecret(spotifySecret),
	)
	httpClient := auth.Client(context.Background(), &token)
	u.client = spotify.New(httpClient, spotify.WithRetry(true))
	return nil
}

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
