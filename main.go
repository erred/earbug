package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"time"

	"cloud.google.com/go/storage"

	"github.com/zmb3/spotify"
	"golang.org/x/oauth2"
)

var (
	// SPOTIFY_ID and SPOTIFY_SECRET
	Bucket      = os.Getenv("BUCKET")
	Token       = os.Getenv("SPOTIFY_TOKEN")
	redirectURL = "http://localhost:8910/auth"
	scopes      = []string{
		spotify.ScopeUserReadRecentlyPlayed,
	}
)

type Client struct {
	spot       *spotify.Client
	lastUpdate time.Time
	// from old to new
	plays map[time.Time]Play
	bkt   *storage.BucketHandle
}

func main() {
	tokp := flag.String("t", "token.json", "path to token file to read/write")
	tick := flag.Duration("i", 15*time.Minute, "interval to update, see time.Duration")
	flag.Parse()

	if flag.Arg(0) == "auth" {
		err := genToken(*tokp)
		if err != nil {
			log.Fatal("generate token: ", err)
		}
		return
	}

	client, err := tokenAuth(Token)
	if err != nil {
		log.Fatal("get client from token: ", err)
	}

	ticker := time.NewTicker(*tick)
	go client.saveListen()
	for range ticker.C {
		go client.saveListen()
	}
}

func (c *Client) saveListen() {
	err := c.getListen()
	if err != nil {
		log.Println("get listen: ", err)
	}
	c.save()
}

func (c *Client) getListen() error {
	timeOut := 30 * time.Second
	for retry := 0; retry < 5; retry++ {
		if retry > 0 {
			log.Printf("attempt %d\n", retry)
		}
		recent, err := c.spot.PlayerRecentlyPlayedOpt(&spotify.RecentlyPlayedOptions{
			Limit:        50,
			AfterEpochMs: c.lastUpdate.UnixNano()/1000 - 1,
		})
		if err != nil {
			sleep := timeOut * time.Duration(int64(retry))
			log.Printf("retrying in %vs\n get recent: %v", sleep.Seconds(), err)
			time.Sleep(sleep)
			continue
		}

		// new->old
		for _, it := range recent {
			c.plays[it.PlayedAt] = PlayFromRecent(it)
		}
		//  success
		return nil
	}

	return fmt.Errorf("getListen failed after retries")
}

type stat struct {
	d       string
	success bool
}

func (c *Client) save() {
	// split c.plays into per day
	plays := map[string]map[time.Time]Play{}
	for t, p := range c.plays {
		d := t.Format("2006-01-02")
		if plays[d] == nil {
			plays[d] = map[time.Time]Play{}
		}
		plays[d][t] = p
	}

	success := make(chan stat, len(plays))
	for d, pm := range plays {
		go func(d string, pm map[time.Time]Play) {
			// get previous for the day
			n := d + ".json"
			ops, err := c.read(n)
			if err != nil {
				log.Printf("read %s err: %v", n, err)
			}

			// set, overwrite old
			m := map[time.Time]Play{}
			for _, p := range ops {
				m[p.Start] = p
			}
			for t, p := range pm {
				m[t] = p
			}

			// serialize in order
			var pl []Play
			for _, p := range m {
				pl = append(pl, p)
			}
			sort.Sort(Plays(pl))

			// write
			err = c.write(n, pl)
			if err != nil {
				success <- stat{d, false}
			}
			success <- stat{d, true}
		}(d, pm)
	}

	// clear c.plays
	c.plays = make(map[time.Time]Play)
	for range plays {
		stat := <-success
		if !stat.success {
			// write back to c.plays on failure
			for t, p := range plays[stat.d] {
				c.plays[t] = p
			}
		}
		delete(plays, stat.d)
	}
}

func (c *Client) write(n string, ps []Play) error {
	w := c.bkt.Object(n).NewWriter(context.Background())
	err := json.NewEncoder(w).Encode(ps)
	if err != nil {
		err2 := w.Close()
		return fmt.Errorf("encoder: %v, writer: %v", err, err2)
	}
	err = w.Close()
	if err != nil {
		return fmt.Errorf("writer: %v", err)
	}
	return nil
}

func (c *Client) read(n string) ([]Play, error) {
	r, err := c.bkt.Object(n).NewReader(context.Background())
	if err != nil {
		return nil, fmt.Errorf("getting reader for %s: %v", n, err)
	}
	defer r.Close()
	var ps []Play
	err = json.NewDecoder(r).Decode(&ps)
	if err != nil {
		return nil, fmt.Errorf("decode reader %s: %v", n, err)
	}
	return ps, nil
}

type Plays []Play

func (p Plays) Len() int           { return len(p) }
func (p Plays) Less(i, j int) bool { return p[i].Start.Before(p[j].Start) }
func (p Plays) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

type Play struct {
	Start     time.Time
	Duration  time.Duration
	TrackID   string
	TrackName string
	Artists   []Artist
}

func PlayFromRecent(it spotify.RecentlyPlayedItem) Play {
	p := Play{
		Start:     it.PlayedAt,
		Duration:  time.Duration(int64(it.Track.Duration)) * time.Millisecond,
		TrackID:   string(it.Track.ID),
		TrackName: it.Track.Name,
	}
	for _, a := range it.Track.Artists {
		p.Artists = append(p.Artists, Artist{ID: string(a.ID), Name: a.Name})
	}
	return p
}

type Artist struct {
	Name string
	ID   string
}

// genToken generates a toke with web flow
func genToken(tokenFile string) error {
	auth := spotify.NewAuthenticator(redirectURL, scopes...)
	url := auth.AuthURL("")
	fmt.Println("open the link in the browser to authenticate: \n", url)

	wait := make(chan spotify.Client)
	http.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		token, err := auth.Token("", r)
		if err != nil {
			http.Error(w, "Couldn't get token", http.StatusNotFound)
			return
		}
		w.Write([]byte(`
<html>
<head>
	<title>Success</title>
</head>
<body>
	<h1 style="text-align:center">Authenticated</h1>
	<h3 style="text-align:center">Please return to the terminal</h3>
</body>
</html>
		`))
		// create a client using the specified token
		wait <- auth.NewClient(token)
	})

	srv := &http.Server{Addr: ":8910"}
	go srv.ListenAndServe()
	defer srv.Shutdown(context.Background())

	client := <-wait

	tok, err := client.Token()
	if err != nil {
		return fmt.Errorf("get client token: %v", err)
	}

	f, err := os.Create(tokenFile)
	if err != nil {
		return fmt.Errorf("open token file: %v", err)
	}
	defer f.Close()
	err = json.NewEncoder(f).Encode(tok)
	if err != nil {
		return fmt.Errorf("encode token to file: %v", err)
	}
	fmt.Printf("\nSaved token to %s\n", tokenFile)
	return nil
}

// tokenAuth creates a client from a token string
func tokenAuth(token string) (*Client, error) {
	tok := oauth2.Token{}
	log.Println("using token", token)
	err := json.Unmarshal([]byte(token), &tok)
	if err != nil {
		return nil, fmt.Errorf("decode token file: %v", err)
	}

	cl, err := storage.NewClient(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get bcket client: %v", err)
	}
	bkt := cl.Bucket(Bucket)

	client := spotify.NewAuthenticator(redirectURL, scopes...).NewClient(&tok)
	return &Client{
		spot:  &client,
		bkt:   bkt,
		plays: make(map[time.Time]Play),
	}, nil
}
