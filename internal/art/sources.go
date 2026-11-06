package art

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// RequestTimeout bounds every HTTP request made by a source.
const RequestTimeout = 5 * time.Second

// Source is an online album art lookup. Lookup returns "" with a nil error
// when nothing suitable was found.
type Source interface {
	Name() string
	Lookup(ctx context.Context, q Query) (string, error)
}

func getJSON(ctx context.Context, client *http.Client, req *http.Request, out any) error {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return &httpError{Status: resp.StatusCode, URL: req.URL.Host + req.URL.Path}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

type httpError struct {
	Status int
	URL    string
}

func (e *httpError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.URL, e.Status) }

func acceptURL(u string) string {
	if PublicURL(u) {
		return u
	}
	return ""
}

// ---------------------------------------------------------------- Spotify

// Spotify searches the Spotify Web API using the Client Credentials flow.
type Spotify struct {
	ClientID     string
	ClientSecret string
	TokenURL     string
	APIURL       string
	Client       *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewSpotify creates a Spotify source.
func NewSpotify(id, secret string, client *http.Client) *Spotify {
	return &Spotify{
		ClientID: id, ClientSecret: secret, Client: client,
		TokenURL: "https://accounts.spotify.com/api/token",
		APIURL:   "https://api.spotify.com",
	}
}

// Name implements Source.
func (s *Spotify) Name() string { return "spotify" }

func (s *Spotify) getToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Now().Before(s.expiry) {
		return s.token, nil
	}
	req, err := http.NewRequest(http.MethodPost, s.TokenURL, strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(s.ClientID+":"+s.ClientSecret)))
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := getJSON(ctx, s.Client, req, &out); err != nil {
		// Never include credentials in errors; getJSON only reports host/path.
		return "", fmt.Errorf("spotify token request failed (check art.spotify credentials): %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("spotify token response had no access_token")
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	s.token = out.AccessToken
	s.expiry = time.Now().Add(ttl - time.Minute)
	return s.token, nil
}

func (s *Spotify) invalidate() {
	s.mu.Lock()
	s.token = ""
	s.mu.Unlock()
}

type spotifyImage struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// pickSpotifyImage prefers 640px, then the smallest image >= 300px, then
// the largest available.
func pickSpotifyImage(imgs []spotifyImage) string {
	for _, im := range imgs {
		if im.Width == 640 {
			return im.URL
		}
	}
	best := ""
	bestW := 0
	for _, im := range imgs {
		if im.Width >= 300 && (best == "" || im.Width < bestW) {
			best, bestW = im.URL, im.Width
		}
	}
	if best != "" {
		return best
	}
	for _, im := range imgs {
		if im.Width >= bestW {
			best, bestW = im.URL, im.Width
		}
	}
	return best
}

func quoteSearch(s string) string { return strings.ReplaceAll(s, `"`, " ") }

// Lookup implements Source.
func (s *Spotify) Lookup(ctx context.Context, q Query) (string, error) {
	artist := q.Artist()
	title := cleanForSearch(q.Title)
	if artist == "" || title == "" {
		return "", nil
	}
	query := fmt.Sprintf(`track:"%s" artist:"%s"`, quoteSearch(title), quoteSearch(artist))
	u := s.APIURL + "/v1/search?" + url.Values{"q": {query}, "type": {"track"}, "limit": {"10"}}.Encode()
	var out struct {
		Tracks struct {
			Items []struct {
				Name    string `json:"name"`
				Artists []struct {
					Name string `json:"name"`
				} `json:"artists"`
				Album struct {
					Name   string         `json:"name"`
					Images []spotifyImage `json:"images"`
				} `json:"album"`
			} `json:"items"`
		} `json:"tracks"`
	}
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := s.getToken(ctx)
		if err != nil {
			return "", err
		}
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		err = getJSON(ctx, s.Client, req, &out)
		if he, ok := err.(*httpError); ok && he.Status == http.StatusUnauthorized && attempt == 0 {
			s.invalidate()
			continue
		}
		if err != nil {
			return "", err
		}
		break
	}
	// Prefer a match on the album, then on the title.
	var titleMatch string
	for _, it := range out.Tracks.Items {
		artistOK := false
		for _, a := range it.Artists {
			if ArtistMatch(q.Artists, a.Name) {
				artistOK = true
				break
			}
		}
		if !artistOK {
			continue
		}
		img := acceptURL(pickSpotifyImage(it.Album.Images))
		if img == "" {
			continue
		}
		if q.Album != "" && SimilarTitle(q.Album, it.Album.Name) {
			return img, nil
		}
		if titleMatch == "" && SimilarTitle(q.Title, it.Name) {
			titleMatch = img
		}
	}
	return titleMatch, nil
}

// cleanForSearch strips noise (feat., remaster markers) but keeps case.
func cleanForSearch(s string) string {
	orig := s
	prev := ""
	for prev != s {
		prev = s
		s = reBracketNoise.ReplaceAllString(s, "")
	}
	s = reDashNoise.ReplaceAllString(s, "")
	s = reFeat.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if s == "" {
		return strings.TrimSpace(orig)
	}
	return s
}

// ---------------------------------------------------------------- iTunes

// ITunes uses the keyless iTunes Search API.
type ITunes struct {
	BaseURL string
	Client  *http.Client
}

// NewITunes creates an iTunes source.
func NewITunes(client *http.Client) *ITunes {
	return &ITunes{BaseURL: "https://itunes.apple.com", Client: client}
}

// Name implements Source.
func (s *ITunes) Name() string { return "itunes" }

type itunesResult struct {
	ArtistName     string `json:"artistName"`
	CollectionName string `json:"collectionName"`
	TrackName      string `json:"trackName"`
	ArtworkURL100  string `json:"artworkUrl100"`
}

func upscaleITunes(u string) string {
	return strings.Replace(u, "100x100bb", "600x600bb", 1)
}

func (s *ITunes) search(ctx context.Context, term, entity string) ([]itunesResult, error) {
	u := s.BaseURL + "/search?" + url.Values{"term": {term}, "media": {"music"}, "entity": {entity}, "limit": {"15"}}.Encode()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	var out struct {
		Results []itunesResult `json:"results"`
	}
	if err := getJSON(ctx, s.Client, req, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// Lookup implements Source.
func (s *ITunes) Lookup(ctx context.Context, q Query) (string, error) {
	artist := q.Artist()
	if artist == "" {
		return "", nil
	}
	if album := cleanForSearch(q.Album); album != "" {
		res, err := s.search(ctx, artist+" "+album, "album")
		if err != nil {
			return "", err
		}
		for _, r := range res {
			if ArtistMatch(q.Artists, r.ArtistName) && SimilarTitle(q.Album, r.CollectionName) {
				if u := acceptURL(upscaleITunes(r.ArtworkURL100)); u != "" {
					return u, nil
				}
			}
		}
	}
	if title := cleanForSearch(q.Title); title != "" {
		res, err := s.search(ctx, artist+" "+title, "song")
		if err != nil {
			return "", err
		}
		for _, r := range res {
			if !ArtistMatch(q.Artists, r.ArtistName) || !SimilarTitle(q.Title, r.TrackName) {
				continue
			}
			// With a known album, only accept the matching album's cover.
			if q.Album != "" && !SimilarTitle(q.Album, r.CollectionName) {
				continue
			}
			if u := acceptURL(upscaleITunes(r.ArtworkURL100)); u != "" {
				return u, nil
			}
		}
	}
	return "", nil
}

// ---------------------------------------------------------------- MusicBrainz

// MusicBrainz searches MusicBrainz and returns a Cover Art Archive URL.
type MusicBrainz struct {
	BaseURL   string
	CAAURL    string
	UserAgent string
	Client    *http.Client
	Interval  time.Duration // minimum spacing between MusicBrainz requests

	mu   sync.Mutex
	last time.Time
}

// NewMusicBrainz creates a MusicBrainz source.
func NewMusicBrainz(userAgent string, client *http.Client) *MusicBrainz {
	return &MusicBrainz{
		BaseURL: "https://musicbrainz.org", CAAURL: "https://coverartarchive.org",
		UserAgent: userAgent, Client: client, Interval: time.Second,
	}
}

// Name implements Source.
func (s *MusicBrainz) Name() string { return "musicbrainz" }

// throttle enforces MusicBrainz's 1 request/second limit across lookups.
func (s *MusicBrainz) throttle(ctx context.Context) error {
	s.mu.Lock()
	wait := time.Until(s.last.Add(s.Interval))
	if wait < 0 {
		wait = 0
	}
	s.last = time.Now().Add(wait)
	s.mu.Unlock()
	if wait == 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func luceneEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return r.Replace(s)
}

type mbCredit struct {
	Name   string `json:"name"`
	Artist struct {
		Name string `json:"name"`
	} `json:"artist"`
}

type mbRelease struct {
	ID           string     `json:"id"`
	Title        string     `json:"title"`
	Score        int        `json:"score"`
	ArtistCredit []mbCredit `json:"artist-credit"`
	ReleaseGroup struct {
		ID string `json:"id"`
	} `json:"release-group"`
}

func creditMatches(q Query, credits []mbCredit) bool {
	var names []string
	for _, c := range credits {
		names = append(names, c.Name, c.Artist.Name)
	}
	for _, n := range names {
		if n != "" && ArtistMatch(q.Artists, n) {
			return true
		}
	}
	return ArtistMatch(q.Artists, strings.Join(names, " & "))
}

func (s *MusicBrainz) get(ctx context.Context, path string, params url.Values, out any) error {
	if err := s.throttle(ctx); err != nil {
		return err
	}
	params.Set("fmt", "json")
	req, _ := http.NewRequest(http.MethodGet, s.BaseURL+path+"?"+params.Encode(), nil)
	req.Header.Set("User-Agent", s.UserAgent)
	req.Header.Set("Accept", "application/json")
	return getJSON(ctx, s.Client, req, out)
}

// coverExists checks whether the Cover Art Archive has a front image.
func (s *MusicBrainz) coverExists(ctx context.Context, u string) bool {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	req.Header.Set("User-Agent", s.UserAgent)
	resp, err := s.Client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (s *MusicBrainz) coverFor(ctx context.Context, releases []mbRelease) string {
	tried := 0
	for _, r := range releases {
		if tried >= 3 || ctx.Err() != nil {
			break
		}
		tried++
		if r.ID != "" {
			if u := s.CAAURL + "/release/" + r.ID + "/front-500"; PublicURL(u) && s.coverExists(ctx, u) {
				return u
			}
		}
		if r.ReleaseGroup.ID != "" {
			if u := s.CAAURL + "/release-group/" + r.ReleaseGroup.ID + "/front-500"; PublicURL(u) && s.coverExists(ctx, u) {
				return u
			}
		}
	}
	return ""
}

// Lookup implements Source.
func (s *MusicBrainz) Lookup(ctx context.Context, q Query) (string, error) {
	artist := q.Artist()
	if artist == "" {
		return "", nil
	}
	if album := cleanForSearch(q.Album); album != "" {
		var out struct {
			Releases []mbRelease `json:"releases"`
		}
		query := fmt.Sprintf(`release:"%s" AND artist:"%s"`, luceneEscape(album), luceneEscape(artist))
		if err := s.get(ctx, "/ws/2/release/", url.Values{"query": {query}, "limit": {"10"}}, &out); err != nil {
			return "", err
		}
		var matches []mbRelease
		for _, r := range out.Releases {
			if r.Score >= 70 && creditMatches(q, r.ArtistCredit) && SimilarTitle(q.Album, r.Title) {
				matches = append(matches, r)
			}
		}
		if u := s.coverFor(ctx, matches); u != "" {
			return u, nil
		}
		return "", nil
	}
	title := cleanForSearch(q.Title)
	if title == "" {
		return "", nil
	}
	var out struct {
		Recordings []struct {
			Title        string      `json:"title"`
			Score        int         `json:"score"`
			ArtistCredit []mbCredit  `json:"artist-credit"`
			Releases     []mbRelease `json:"releases"`
		} `json:"recordings"`
	}
	query := fmt.Sprintf(`recording:"%s" AND artist:"%s"`, luceneEscape(title), luceneEscape(artist))
	if err := s.get(ctx, "/ws/2/recording/", url.Values{"query": {query}, "limit": {"10"}}, &out); err != nil {
		return "", err
	}
	var matches []mbRelease
	for _, rec := range out.Recordings {
		if rec.Score >= 70 && creditMatches(q, rec.ArtistCredit) && SimilarTitle(q.Title, rec.Title) {
			matches = append(matches, rec.Releases...)
		}
	}
	return s.coverFor(ctx, matches), nil
}
