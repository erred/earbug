package serve

import (
	"bytes"
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	earbugv4 "go.seankhliao.com/proto/earbug/v4"
	"go.seankhliao.com/webstyle"
)

// /artists?sort=tracks
// /artists?sort=plays
// /artists?sort=time
// /playbacks
// /tracks?sort=plays
// /tracks?sort=time

func optionsFromRequest(r *http.Request) getPlaybacksOptions {
	o := getPlaybacksOptions{
		Artist: r.FormValue("artist"),
		Track:  r.FormValue("track"),
	}
	if t := r.FormValue("from"); t != "" {
		ts, err := time.Parse(time.RFC3339, t)
		if err == nil {
			o.From = ts
		}
	}
	if t := r.FormValue("to"); t != "" {
		ts, err := time.Parse(time.RFC3339, t)
		if err == nil {
			o.To = ts
		}
	}
	return o
}

func (s *Server) handleIndex(rw http.ResponseWriter, r *http.Request) {
	ctx, span := s.o.T.Start(r.Context(), "handleIndex")
	defer span.End()

	if r.URL.Path != "/" {
		http.Redirect(rw, r, "/", http.StatusFound)
		return
	}

	c := `
# earbug

## spotify

### _earbug_

- [artists by track](/artists?sort=track)
- [artists by plays](/artists?sort=plays)
- [artists by time](/artists?sort=time)
- [playbacks](/playbacks)
- [tracks by plays](/tracks?sort=plays)
- [tracks by time](/tracks?sort=time)
`

	err := s.render.Render(rw, strings.NewReader(c), webstyle.Data{})
	if err != nil {
		s.o.HTTPErr(ctx, "render", err, rw, http.StatusInternalServerError)
	}
}

func (s *Server) handleArtists(rw http.ResponseWriter, r *http.Request) {
	ctx, span := s.o.T.Start(r.Context(), "handleArtists")
	defer span.End()

	sortOrder := r.FormValue("sort")
	if sortOrder == "" {
		sortOrder = "plays"
	}

	plays := s.getPlaybacks(ctx, optionsFromRequest(r))

	type TrackData struct {
		ID    string
		Name  string
		Plays int
		Time  time.Duration
	}
	type ArtistData struct {
		Name   string
		Plays  int
		Time   time.Duration
		Tracks []TrackData
	}

	artistIdx := make(map[string]int)
	artistData := []ArtistData{}

	for _, play := range plays {
		for _, artist := range play.Track.Artists {
			idx, ok := artistIdx[artist.Id]
			if !ok {
				artistIdx[artist.Id] = len(artistData)
				idx = len(artistData)
				artistData = append(artistData, ArtistData{
					Name: artist.Name,
				})
			}
			artistData[idx].Plays += 1
			artistData[idx].Time += play.PlaybackTime

			var foundTrack bool
			for i, track := range artistData[idx].Tracks {
				if track.ID == play.Track.Id {
					foundTrack = true
					artistData[idx].Tracks[i].Plays += 1
					artistData[idx].Tracks[i].Time += play.PlaybackTime
				}
			}
			if !foundTrack {
				artistData[idx].Tracks = append(artistData[idx].Tracks, TrackData{
					ID:    play.Track.Id,
					Name:  play.Track.Name,
					Plays: 1,
					Time:  play.PlaybackTime,
				})
			}
		}
	}

	sort.Slice(artistData, func(i, j int) bool {
		switch sortOrder {
		case "tracks":
			if len(artistData[i].Tracks) == len(artistData[j].Tracks) {
				return artistData[i].Name < artistData[j].Name
			}
			return len(artistData[i].Tracks) > len(artistData[j].Tracks)
		case "time":
			return artistData[i].Time < artistData[j].Time
		case "plays":
			fallthrough
		default:
			if artistData[i].Plays == artistData[j].Plays {
				return artistData[i].Name < artistData[j].Name
			}
			return artistData[i].Plays > artistData[j].Plays
		}
	})
	for _, artist := range artistData {
		sort.Slice(artist.Tracks, func(i, j int) bool {
			switch sortOrder {
			case "time":
				return artist.Tracks[i].Time > artist.Tracks[j].Time
			case "plays":
				fallthrough
			default:
				if artist.Tracks[i].Plays == artist.Tracks[j].Plays {
					return artist.Tracks[i].Name < artist.Tracks[j].Name
				}
				return artist.Tracks[i].Plays > artist.Tracks[j].Plays
			}
		})
	}

	var buf bytes.Buffer
	buf.WriteString(`### Artists by `)
	buf.WriteString(sortOrder)
	buf.WriteString("\n\n")
	buf.WriteString("<table><thead><tr><th>artist<th>total<th>track<th>plays<th>time</tr></thead>\n<tbody>")
	for _, artist := range artistData {
		buf.WriteString("<tr><td rowspan=\"")
		buf.WriteString(strconv.Itoa(len(artist.Tracks)))
		buf.WriteString("\">")
		buf.WriteString(artist.Name)
		buf.WriteString("<td rowspan=\"")
		buf.WriteString(strconv.Itoa(len(artist.Tracks)))
		buf.WriteString("\">")
		switch sortOrder {
		case "tracks":
			buf.WriteString(strconv.Itoa(len(artist.Tracks)))
		case "time":
			buf.WriteString(artist.Time.String())
		case "plays":
			fallthrough
		default:
			buf.WriteString(strconv.Itoa(artist.Plays))
		}
		for i, track := range artist.Tracks {
			if i != 0 {
				buf.WriteString("</tr>\n<tr>")
			}
			buf.WriteString("<td>")
			buf.WriteString(track.Name)
			buf.WriteString("<td>")
			buf.WriteString(strconv.Itoa(track.Plays))
			buf.WriteString("<td>")
			buf.WriteString(track.Time.String())
		}
		buf.WriteString("</tr>\n")
	}
	buf.WriteString("</tbody></table>")

	err := s.render.Render(rw, &buf, webstyle.Data{})
	if err != nil {
		s.o.HTTPErr(ctx, "render", err, rw, http.StatusInternalServerError)
		return
	}
}

func (s *Server) handleTracks(rw http.ResponseWriter, r *http.Request) {
	ctx, span := s.o.T.Start(r.Context(), "handleTracks")
	defer span.End()

	sortOrder := r.FormValue("sort")
	if sortOrder == "" {
		sortOrder = "plays"
	}

	plays := s.getPlaybacks(ctx, optionsFromRequest(r))

	type TrackData struct {
		Name    string
		Plays   int
		Time    time.Duration
		Artists []*earbugv4.Artist
	}

	trackIdx := make(map[string]int)
	trackData := []TrackData{}

	for _, play := range plays {
		idx, ok := trackIdx[play.Track.Id]
		if !ok {
			trackIdx[play.Track.Id] = len(trackData)
			idx = len(trackData)
			trackData = append(trackData, TrackData{
				Name:    play.Track.Name,
				Artists: play.Track.Artists,
			})
		}
		trackData[idx].Plays += 1
		trackData[idx].Time += play.PlaybackTime
	}

	sort.Slice(trackData, func(i, j int) bool {
		switch sortOrder {
		case "time":
			return trackData[i].Time > trackData[j].Time
		case "plays":
			fallthrough
		default:
			if trackData[i].Plays == trackData[j].Plays {
				return trackData[i].Name < trackData[j].Name
			}
			return trackData[i].Plays > trackData[j].Plays
		}
	})

	var buf bytes.Buffer
	buf.WriteString(`### Tracks by `)
	buf.WriteString(sortOrder)
	buf.WriteString("\n\n")
	buf.WriteString("<table><thead><tr><th>track<th>plays<th>time<th>artists</tr></thead>\n<tbody>")
	for _, track := range trackData {
		var artists []string
		for _, artist := range track.Artists {
			artists = append(artists, artist.Name)
		}
		buf.WriteString("<tr><td>")
		buf.WriteString(track.Name)
		buf.WriteString("<td>")
		buf.WriteString(strconv.Itoa(track.Plays))
		buf.WriteString("<td>")
		buf.WriteString(track.Time.String())
		buf.WriteString("<td>")
		buf.WriteString(strings.Join(artists, ", "))
		buf.WriteString("</tr>\n")
	}
	buf.WriteString("</tbody></table>")

	err := s.render.Render(rw, &buf, webstyle.Data{})
	if err != nil {
		s.o.HTTPErr(ctx, "render", err, rw, http.StatusInternalServerError)
		return
	}
}

func (s *Server) handlePlaybacks(rw http.ResponseWriter, r *http.Request) {
	ctx, span := s.o.T.Start(r.Context(), "handlePlaybacks")
	defer span.End()

	plays := s.getPlaybacks(ctx, optionsFromRequest(r))

	var buf bytes.Buffer
	buf.WriteString(`### Playbacks `)
	buf.WriteString("\n\n")
	buf.WriteString("<table><thead><tr><th>time<th>duration<th>track<th>artists</tr></thead>\n<tbody>")
	for _, play := range plays {
		var artists []string
		for _, artist := range play.Track.Artists {
			artists = append(artists, artist.Name)
		}
		buf.WriteString("<tr><td>")
		buf.WriteString(play.StartTime.String())
		buf.WriteString("<td>")
		buf.WriteString(play.PlaybackTime.String())
		buf.WriteString("<td>")
		buf.WriteString(play.Track.Name)
		buf.WriteString("<td>")
		buf.WriteString(strings.Join(artists, ", "))
		buf.WriteString("</tr>\n")
	}

	buf.WriteString("</tbody></table>")

	err := s.render.Render(rw, &buf, webstyle.Data{})
	if err != nil {
		s.o.HTTPErr(ctx, "render", err, rw, http.StatusInternalServerError)
		return
	}
}

type getPlaybacksOptions struct {
	From time.Time
	To   time.Time

	Artist string
	Track  string
}

type Playback struct {
	StartTime    time.Time
	PlaybackTime time.Duration
	Track        *earbugv4.Track
}

func (s *Server) getPlaybacks(ctx context.Context, o getPlaybacksOptions) []Playback {
	_, span := s.o.T.Start(ctx, "getPlaybacks")
	defer span.End()

	var plays []Playback

	s.storemu.Lock()
	defer s.storemu.Unlock()
	for ts, play := range s.store.Playbacks {
		startTime, _ := time.Parse(time.RFC3339, ts)

		if !o.From.IsZero() && o.From.After(startTime) {
			continue
		} else if !o.To.IsZero() && o.To.Before(startTime) {
			continue
		}

		track := s.store.Tracks[play.TrackId]

		if o.Track != "" && !strings.Contains(strings.ToLower(track.Name), strings.ToLower(o.Track)) {
			continue
		}

		artistMatch := o.Artist == ""
		for _, artist := range track.Artists {
			if !artistMatch && strings.Contains(strings.ToLower(artist.Name), strings.ToLower(o.Artist)) {
				artistMatch = true
			}
		}
		if !artistMatch {
			continue
		}

		plays = append(plays, Playback{
			StartTime: startTime,
			Track:     track,
		})
	}

	sort.Slice(plays, func(i, j int) bool {
		return plays[i].StartTime.After(plays[j].StartTime)
	})

	for i := range plays {
		plays[i].PlaybackTime = plays[i].Track.Duration.AsDuration()
		if i > 0 {
			gap := plays[i-1].StartTime.Sub(plays[i].StartTime)
			if gap < plays[i].PlaybackTime {
				plays[i].PlaybackTime = gap
			}
		}
	}

	return plays
}
