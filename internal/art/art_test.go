package art

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"Song (feat. Someone)":                 "song",
		"Song [Explicit]":                      "song",
		"Here Comes the Sun - Remastered 2009": "here comes the sun",
		"Abbey Road (Super Deluxe Edition)":    "abbey road",
		"Help! (2009 Remaster)":                "help",
		"Song ft. Other Person":                "song",
		"Café del Mar":                         "cafe del mar",
		"Rock & Roll":                          "rock and roll",
		"  Multiple   Spaces ":                 "multiple spaces",
		"Live at Leeds":                        "live at leeds",
		"(Explicit)":                           "explicit",
		"Song - Single":                        "song",
		"Hello - Radio Edit":                   "hello",
		"Don't Stop Me Now":                    "don t stop me now",
	}
	for in, want := range cases {
		if got := NormalizeTitle(in); got != want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
	if NormalizeArtist("The Beatles") != "beatles" || NormalizeArtist("Beyoncé") != "beyonce" {
		t.Error("artist normalization")
	}
}

func TestSimilarAndArtistMatch(t *testing.T) {
	yes := [][2]string{
		{"Abbey Road", "Abbey Road (Remastered)"},
		{"Abbey Road", "abbey road"},
		{"Random Access Memories", "Random Access Memories [10th Anniversary Edition]"},
		{"The Dark Side of the Moon", "Dark Side of the Moon"},
		{"Song (feat. X)", "Song"},
	}
	for _, p := range yes {
		if !SimilarTitle(p[0], p[1]) {
			t.Errorf("%q ~ %q should match", p[0], p[1])
		}
	}
	no := [][2]string{
		{"Help", "Help Me Rhonda"},
		{"Love", "Love Me Do"},
		{"Abbey Road", "Let It Be"},
		{"Greatest Hits", "Greatest Hits Volume 2 The Remixes Collection"},
		{"", "x"},
	}
	for _, p := range no {
		if SimilarTitle(p[0], p[1]) {
			t.Errorf("%q ~ %q should NOT match", p[0], p[1])
		}
	}
	if !ArtistMatch([]string{"The Beatles"}, "Beatles") {
		t.Error("the-prefix")
	}
	if !ArtistMatch([]string{"Daft Punk"}, "Daft Punk feat. Pharrell Williams") {
		t.Error("featuring credit")
	}
	if !ArtistMatch([]string{"Simon & Garfunkel"}, "Simon and Garfunkel") {
		t.Error("ampersand")
	}
	if !ArtistMatch([]string{"A", "Pharrell Williams"}, "Pharrell Williams") {
		t.Error("second artist")
	}
	if ArtistMatch([]string{"Queen"}, "Queens of the Stone Age") {
		t.Error("Queen != QOTSA")
	}
	if ArtistMatch([]string{"Muse"}, "Various Artists") {
		t.Error("various artists")
	}
}

func TestPublicURL(t *testing.T) {
	good := []string{
		"https://i.scdn.co/image/ab67616d0000b273abc",
		"http://example.com/a.jpg",
		"https://8.8.8.8/x.png",
	}
	bad := []string{
		"", "file:///home/me/cover.jpg", "data:image/png;base64,AAAA",
		"http://localhost:8080/a.jpg", "http://127.0.0.1/a.jpg", "http://192.168.1.5/a.jpg",
		"http://10.0.0.1/a.jpg", "http://[::1]/a.jpg", "http://nas.local/a.jpg", "http://intranet/a.jpg",
		"http://100.64.1.1/a.jpg", "ftp://example.com/a.jpg", "https://user:pw@example.com/a.jpg",
		"https://example.com/" + strings.Repeat("a", 250),
	}
	for _, u := range good {
		if !PublicURL(u) {
			t.Errorf("%q should be public", u)
		}
	}
	for _, u := range bad {
		if PublicURL(u) {
			t.Errorf("%q should be rejected", u)
		}
	}
}

func TestCachePersistenceAndTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "art.json")
	c := OpenCache(path)
	now := time.Now()
	c.now = func() time.Time { return now }
	if err := c.Put("k1", "https://x.example/a.jpg", "itunes", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("neg", "", "", time.Minute); err != nil {
		t.Fatal(err)
	}
	c2 := OpenCache(path)
	c2.now = func() time.Time { return now }
	if e, ok := c2.Get("k1"); !ok || e.URL != "https://x.example/a.jpg" || e.Source != "itunes" {
		t.Fatalf("persisted entry: %+v %v", e, ok)
	}
	if e, ok := c2.Get("neg"); !ok || e.URL != "" {
		t.Fatalf("negative entry: %+v %v", e, ok)
	}
	c2.now = func() time.Time { return now.Add(2 * time.Minute) }
	if _, ok := c2.Get("neg"); ok {
		t.Fatal("negative entry should expire")
	}
	if _, ok := c2.Get("k1"); !ok {
		t.Fatal("positive entry expired early")
	}
}

type fakeSource struct {
	name  string
	url   string
	err   error
	calls atomic.Int32
}

func (f *fakeSource) Name() string { return f.name }
func (f *fakeSource) Lookup(context.Context, Query) (string, error) {
	f.calls.Add(1)
	return f.url, f.err
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestResolverChain(t *testing.T) {
	q := Query{Title: "Song", Artists: []string{"Artist"}, Album: "Album", ArtURL: "file:///tmp/x.jpg"}
	sp := &fakeSource{name: "spotify", err: errors.New("down")}
	it := &fakeSource{name: "itunes"}
	mb := &fakeSource{name: "musicbrainz", url: "https://coverartarchive.org/release/x/front-500"}
	cache := OpenCache(filepath.Join(t.TempDir(), "art.json"))
	r := NewResolverWith([]string{"mpris", "spotify", "itunes", "musicbrainz"},
		map[string]Source{"spotify": sp, "itunes": it, "musicbrainz": mb}, cache, quiet())

	if _, done := r.Immediate(q); done {
		t.Fatal("file:// art should require a lookup")
	}
	if got := r.Lookup(context.Background(), q); got != mb.url {
		t.Fatalf("got %q", got)
	}
	if sp.calls.Load() != 1 || it.calls.Load() != 1 || mb.calls.Load() != 1 {
		t.Fatal("chain order not followed")
	}
	// Second time: served from cache without network.
	if u, done := r.Immediate(q); !done || u != mb.url {
		t.Fatalf("cache: %q %v", u, done)
	}
	// Public mpris art wins immediately.
	q2 := q
	q2.ArtURL = "https://i.scdn.co/image/abc"
	if u, done := r.Immediate(q2); !done || u != q2.ArtURL {
		t.Fatalf("mpris: %q %v", u, done)
	}
	// Negative results are cached.
	q3 := Query{Title: "Nothing", Artists: []string{"Nobody"}}
	mb.url = ""
	if got := r.Lookup(context.Background(), q3); got != "" {
		t.Fatal(got)
	}
	before := it.calls.Load()
	if u, done := r.Immediate(q3); !done || u != "" {
		t.Fatalf("negative cache: %q %v", u, done)
	}
	r.Lookup(context.Background(), q3)
	if it.calls.Load() != before {
		t.Fatal("negative result not cached")
	}
	// Unsearchable metadata needs no lookup.
	if u, done := r.Immediate(Query{Title: "x"}); !done || u != "" {
		t.Fatal("unsearchable should be done")
	}
}

func TestResolverRejectsLongURL(t *testing.T) {
	long := &fakeSource{name: "itunes", url: "https://example.com/" + strings.Repeat("a", 300)}
	r := NewResolverWith([]string{"itunes"}, map[string]Source{"itunes": long}, nil, quiet())
	if got := r.Lookup(context.Background(), Query{Title: "t", Artists: []string{"a"}}); got != "" {
		t.Fatalf("long url accepted: %q", got)
	}
}

func TestITunesSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("entity") == "album" {
			fmt.Fprint(w, `{"results":[
				{"artistName":"Someone Else","collectionName":"Abbey Road","artworkUrl100":"https://is1.mzstatic.com/wrong/100x100bb.jpg"},
				{"artistName":"The Beatles","collectionName":"Abbey Road (Remastered)","artworkUrl100":"https://is1.mzstatic.com/right/100x100bb.jpg"}]}`)
			return
		}
		fmt.Fprint(w, `{"results":[]}`)
	}))
	defer srv.Close()
	s := NewITunes(srv.Client())
	s.BaseURL = srv.URL
	got, err := s.Lookup(context.Background(), Query{Title: "Come Together", Artists: []string{"The Beatles"}, Album: "Abbey Road"})
	if err != nil || got != "https://is1.mzstatic.com/right/600x600bb.jpg" {
		t.Fatalf("%q %v", got, err)
	}
	got, err = s.Lookup(context.Background(), Query{Title: "X", Artists: []string{"Nobody"}, Album: "Nothing"})
	if err != nil || got != "" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestSpotifySource(t *testing.T) {
	var tokenCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/token":
			tokenCalls.Add(1)
			if u, p, ok := r.BasicAuth(); !ok || u != "id" || p != "secret" {
				w.WriteHeader(401)
				return
			}
			fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
		case "/v1/search":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(401)
				return
			}
			q := r.URL.Query().Get("q")
			if !strings.Contains(q, `track:"Get Lucky"`) || !strings.Contains(q, `artist:"Daft Punk"`) {
				t.Errorf("query %q", q)
			}
			fmt.Fprint(w, `{"tracks":{"items":[{"name":"Get Lucky (feat. Pharrell Williams)","artists":[{"name":"Daft Punk"}],
				"album":{"name":"Random Access Memories","images":[
				{"url":"https://i.scdn.co/image/300","width":300,"height":300},
				{"url":"https://i.scdn.co/image/640","width":640,"height":640},
				{"url":"https://i.scdn.co/image/64","width":64,"height":64}]}}]}}`)
		}
	}))
	defer srv.Close()
	s := NewSpotify("id", "secret", srv.Client())
	s.TokenURL = srv.URL + "/api/token"
	s.APIURL = srv.URL
	q := Query{Title: "Get Lucky (feat. Pharrell Williams)", Artists: []string{"Daft Punk"}, Album: "Random Access Memories"}
	for i := 0; i < 2; i++ {
		got, err := s.Lookup(context.Background(), q)
		if err != nil || got != "https://i.scdn.co/image/640" {
			t.Fatalf("%q %v", got, err)
		}
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("token not cached: %d calls", tokenCalls.Load())
	}
	bad := NewSpotify("id", "wrong-secret", srv.Client())
	bad.TokenURL = srv.URL + "/api/token"
	bad.APIURL = srv.URL
	_, err := bad.Lookup(context.Background(), q)
	if err == nil || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("error must exist and not leak the secret: %v", err)
	}
	if pickSpotifyImage([]spotifyImage{{URL: "a", Width: 1000}, {URL: "b", Width: 300}}) != "b" {
		t.Error("prefer 300 when no 640")
	}
}

func TestMusicBrainzSource(t *testing.T) {
	var lastMB time.Time
	minGap := time.Hour
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/ws/2/release"):
			if !strings.HasPrefix(r.Header.Get("User-Agent"), "tunecord/") {
				t.Errorf("UA %q", r.Header.Get("User-Agent"))
			}
			if !lastMB.IsZero() && time.Since(lastMB) < minGap {
				minGap = time.Since(lastMB)
			}
			lastMB = time.Now()
			fmt.Fprint(w, `{"releases":[
				{"id":"wrong","title":"Other","score":100,"artist-credit":[{"name":"Artist"}]},
				{"id":"nocover","title":"Album","score":95,"artist-credit":[{"name":"Artist"}],"release-group":{"id":"rg1"}}]}`)
		case r.URL.Path == "/release/nocover/front-500":
			w.WriteHeader(404)
		case r.URL.Path == "/release-group/rg1/front-500":
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	s := NewMusicBrainz("tunecord/test (https://example.com)", srv.Client())
	s.BaseURL = srv.URL
	// The test server is on localhost, which PublicURL rejects; use a
	// public-looking CAA base whose requests are routed to the test server.
	s.CAAURL = "http://caa.example.com"
	s.Client = &http.Client{Transport: rewriteTransport{target: srv.URL}}
	s.Interval = 200 * time.Millisecond
	q := Query{Title: "Song", Artists: []string{"Artist"}, Album: "Album"}
	for i := 0; i < 2; i++ {
		got, err := s.Lookup(context.Background(), q)
		if err != nil || got != "http://caa.example.com/release-group/rg1/front-500" {
			t.Fatalf("%q %v", got, err)
		}
	}
	if minGap < 150*time.Millisecond {
		t.Fatalf("rate limit not respected: %v", minGap)
	}
}

type rewriteTransport struct{ target string }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u := *r.URL
	tu, _ := http.NewRequest(r.Method, rt.target, nil)
	u.Scheme, u.Host = tu.URL.Scheme, tu.URL.Host
	r2 := r.Clone(r.Context())
	r2.URL = &u
	r2.Host = u.Host
	return http.DefaultTransport.RoundTrip(r2)
}
