// Package art resolves album art URLs for Discord through a chain of
// sources (MPRIS artUrl, Spotify, iTunes, MusicBrainz/Cover Art Archive)
// with fuzzy-but-careful matching and a persistent cache.
package art

import (
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// MaxURLLen is Discord's limit for image URLs.
const MaxURLLen = 256

// Query describes the track to find art for.
type Query struct {
	Title   string
	Artists []string
	Album   string
	ArtURL  string // mpris:artUrl
}

// Artist returns the primary artist.
func (q Query) Artist() string {
	for _, a := range q.Artists {
		if a = strings.TrimSpace(a); a != "" {
			return a
		}
	}
	return ""
}

// Searchable reports whether there is enough metadata for an online lookup.
func (q Query) Searchable() bool {
	return NormalizeArtist(q.Artist()) != "" && (NormalizeTitle(q.Album) != "" || NormalizeTitle(q.Title) != "")
}

// CacheKey is the normalized artist+album key, falling back to artist+title.
func (q Query) CacheKey() string {
	a := NormalizeArtist(q.Artist())
	if al := NormalizeTitle(q.Album); al != "" {
		return "album|" + a + "|" + al
	}
	return "track|" + a + "|" + NormalizeTitle(q.Title)
}

var (
	noiseWords = `feat|ft|featuring|with|remaster|remastered|remasterd|explicit|clean|deluxe|edition|version|bonus|expanded|anniversary|mono|stereo|single|ep|reissue|bonus tracks?` //nolint:misspell // "remasterd" is a real-world typo in track titles
	// "(feat. X)", "[Explicit]", "(2011 Remaster)", "{Deluxe Edition}"
	reBracketNoise = regexp.MustCompile(`(?i)\s*[\(\[\{][^\)\]\}]*\b(` + noiseWords + `)\b[^\)\]\}]*[\)\]\}]`)
	// "- Remastered 2011", " - 2009 Remaster", " - Single", " - Radio Edit"
	reDashNoise = regexp.MustCompile(`(?i)\s+[-–—]\s+[^-–—]*\b(` + noiseWords + `|radio edit|edit)\b.*$`)
	// "Song feat. Someone" without brackets
	reFeat     = regexp.MustCompile(`(?i)\s+(feat\.?|ft\.?|featuring)\s+.*$`)
	reNonAlnum = regexp.MustCompile(`[^\p{L}\p{N}]+`)
	reArtSplit = regexp.MustCompile(`(?i)\s*(,|;|/|&|\+|\bx\b|\band\b|\bfeat\.?|\bft\.?|\bfeaturing\b|\bwith\b|\bvs\.?)\s*`)
)

func fold(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func basic(s string) string {
	s = strings.ToLower(fold(s))
	s = strings.ReplaceAll(s, "&", " and ")
	s = reNonAlnum.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

// NormalizeTitle normalizes a track or album title for matching: case and
// diacritics are folded, and featuring credits, remaster/edition/explicit
// markers and punctuation are removed.
func NormalizeTitle(s string) string {
	orig := s
	prev := ""
	for prev != s {
		prev = s
		s = reBracketNoise.ReplaceAllString(s, "")
	}
	s = reDashNoise.ReplaceAllString(s, "")
	s = reFeat.ReplaceAllString(s, "")
	out := basic(s)
	if out == "" {
		return basic(orig)
	}
	return out
}

// NormalizeArtist normalizes an artist name ("The Beatles" -> "beatles").
func NormalizeArtist(s string) string {
	s = basic(s)
	s = strings.TrimPrefix(s, "the ")
	return s
}

// splitArtists splits a credit like "A feat. B & C" into normalized names.
func splitArtists(s string) []string {
	var out []string
	for _, p := range reArtSplit.Split(s, -1) {
		if n := NormalizeArtist(p); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func tokens(s string) map[string]bool {
	m := map[string]bool{}
	for _, f := range strings.Fields(s) {
		m[f] = true
	}
	return m
}

// similarNorm compares two already-normalized strings.
func similarNorm(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	short, long := a, b
	if len(short) > len(long) {
		short, long = long, short
	}
	// Containment on word boundaries, only when the shorter string is
	// substantial relative to the longer one.
	if len([]rune(short)) >= 4 && float64(len(short))/float64(len(long)) >= 0.6 {
		if strings.HasPrefix(long, short+" ") || strings.HasSuffix(long, " "+short) || strings.Contains(long, " "+short+" ") {
			return true
		}
	}
	ta, tb := tokens(a), tokens(b)
	inter := 0
	for t := range ta {
		if tb[t] {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	return union > 0 && len(ta) >= 2 && len(tb) >= 2 && float64(inter)/float64(union) >= 0.8
}

// SimilarTitle reports whether two titles refer to the same work.
func SimilarTitle(a, b string) bool {
	return similarNorm(NormalizeTitle(a), NormalizeTitle(b))
}

// ArtistMatch reports whether a result artist credit matches any of the
// query artists.
func ArtistMatch(queryArtists []string, result string) bool {
	rn := NormalizeArtist(result)
	rparts := splitArtists(result)
	for _, qa := range queryArtists {
		qn := NormalizeArtist(qa)
		if qn == "" {
			continue
		}
		if similarNorm(qn, rn) {
			return true
		}
		for _, qp := range splitArtists(qa) {
			for _, rp := range rparts {
				if qp == rp {
					return true
				}
			}
		}
	}
	return false
}

// PublicURL reports whether u is an http(s) URL that Discord's servers can
// fetch: not file://, data:, localhost or private-network, and short enough.
func PublicURL(u string) bool {
	if u == "" || len(u) > MaxURLLen {
		return false
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.User != nil {
		return false
	}
	host := strings.ToLower(p.Hostname())
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		nonPublic := ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			ip.IsUnspecified() || ip.IsMulticast() || cgnat.Contains(ip)
		return !nonPublic
	}
	if !strings.Contains(host, ".") || host == "localhost" {
		return false
	}
	for _, suf := range []string{".localhost", ".local", ".lan", ".home", ".internal", ".localdomain", ".home.arpa"} {
		if strings.HasSuffix(host, suf) {
			return false
		}
	}
	return true
}

var cgnat = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }()
