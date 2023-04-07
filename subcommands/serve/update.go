package serve

import (
	"context"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/zmb3/spotify/v2"
	earbugv4 "go.seankhliao.com/proto/earbug/v4"
	"google.golang.org/protobuf/types/known/durationpb"
)

func (s *Server) UpdateRecentlyPlayed(ctx context.Context, r *connect.Request[earbugv4.UpdateRecentlyPlayedRequest]) (*connect.Response[earbugv4.UpdateRecentlyPlayedResponse], error) {
	_, span := s.o.T.Start(ctx, "UpdateRecentlyPlayed")
	defer span.End()

	items, err := s.spot.PlayerRecentlyPlayedOpt(ctx, &spotify.RecentlyPlayedOptions{Limit: 50})
	if err != nil {
		return nil, s.o.Err(ctx, "get recently played", err)
	}

	for _, item := range items {
		ts := item.PlayedAt.Format(time.RFC3339Nano)
		if _, ok := s.store.Playbacks[ts]; !ok {
			s.store.Playbacks[ts] = &earbugv4.Playback{
				TrackId:     item.Track.ID.String(),
				TrackUri:    string(item.Track.URI),
				ContextType: item.PlaybackContext.Type,
				ContextUri:  string(item.PlaybackContext.URI),
			}
		}

		if _, ok := s.store.Tracks[item.Track.ID.String()]; !ok {
			t := &earbugv4.Track{
				Id:       item.Track.ID.String(),
				Uri:      string(item.Track.URI),
				Type:     item.Track.Type,
				Name:     item.Track.Name,
				Duration: durationpb.New(item.Track.TimeDuration()),
			}
			for _, artist := range item.Track.Artists {
				t.Artists = append(t.Artists, &earbugv4.Artist{
					Id:   artist.ID.String(),
					Uri:  string(artist.URI),
					Name: artist.Name,
				})
			}
			s.store.Tracks[item.Track.ID.String()] = t
		}
	}
	return &connect.Response[earbugv4.UpdateRecentlyPlayedResponse]{}, nil
}
