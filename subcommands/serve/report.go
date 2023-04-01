package serve

import (
	"context"
	"sort"
	"time"

	"github.com/bufbuild/connect-go"
	earbugv4 "go.seankhliao.com/proto/earbug/v4"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Server) ReportPlayed(ctx context.Context, r *connect.Request[earbugv4.ReportPlayedRequest]) (*connect.Response[earbugv4.ReportPlayedResponse], error) {
	_, span := s.o.T.Start(ctx, "ReportPlayed")
	defer span.End()

	since := r.Msg.Since.AsTime().Format(time.RFC3339)
	var plays []*earbugv4.ReportPlayedResponse_Playback

	s.storemu.Lock()
	for ts, play := range s.store.Playbacks {
		if ts < since {
			continue
		}
		startTime, _ := time.Parse(time.RFC3339, ts)

		track := s.store.Tracks[play.TrackId]
		var artists []*earbugv4.ReportPlayedResponse_Artist
		for _, artist := range track.Artists {
			artists = append(artists, &earbugv4.ReportPlayedResponse_Artist{
				Id:   artist.Id,
				Name: artist.Name,
			})
		}
		plays = append(plays, &earbugv4.ReportPlayedResponse_Playback{
			StartTime: timestamppb.New(startTime),
			Track: &earbugv4.ReportPlayedResponse_Track{
				Id:   track.Id,
				Name: track.Name,
			},
			Artists: artists,
		})
	}
	s.storemu.Unlock()

	sort.Slice(plays, func(i, j int) bool {
		return plays[i].StartTime.AsTime().After(plays[j].StartTime.AsTime())
	})

	return &connect.Response[earbugv4.ReportPlayedResponse]{
		Msg: &earbugv4.ReportPlayedResponse{
			Playbacks: plays,
		},
	}, nil
}
