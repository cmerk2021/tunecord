package art

import (
	"context"
	"log/slog"
	"net/http"

	"tunecord/internal/config"
)

// Resolver runs the album art resolution chain.
type Resolver struct {
	order   []string
	sources map[string]Source
	cache   *Cache
	log     *slog.Logger
}

// NewResolver builds a resolver from the art config. Spotify is only
// enabled when credentials are configured.
func NewResolver(cfg config.Art, userAgent string, cache *Cache, log *slog.Logger) *Resolver {
	client := &http.Client{Timeout: RequestTimeout}
	sources := map[string]Source{
		"itunes":      NewITunes(client),
		"musicbrainz": NewMusicBrainz(userAgent, client),
	}
	if cfg.Spotify.Enabled() {
		sources["spotify"] = NewSpotify(cfg.Spotify.ClientID, cfg.Spotify.ClientSecret, client)
	}
	return NewResolverWith(cfg.Sources, sources, cache, log)
}

// NewResolverWith builds a resolver with explicit sources (used by tests).
// order lists source names; "mpris" refers to the player's own artUrl.
func NewResolverWith(order []string, sources map[string]Source, cache *Cache, log *slog.Logger) *Resolver {
	if cache == nil {
		cache = OpenCache("")
	}
	if log == nil {
		log = slog.Default()
	}
	var o []string
	for _, name := range order {
		if name == "mpris" || sources[name] != nil {
			o = append(o, name)
		}
	}
	return &Resolver{order: o, sources: sources, cache: cache, log: log}
}

// Immediate resolves without network access: a usable mpris:artUrl, a
// cached result, or a definitive "nothing to look up". done is false when an
// asynchronous Lookup is needed.
func (r *Resolver) Immediate(q Query) (url string, done bool) {
	searchable := q.Searchable()
	var cached Entry
	var hasCached bool
	if searchable {
		cached, hasCached = r.cache.Get(q.CacheKey())
	}
	for _, name := range r.order {
		if name == "mpris" {
			if PublicURL(q.ArtURL) {
				return q.ArtURL, true
			}
			continue
		}
		if !searchable {
			continue
		}
		if !hasCached {
			return "", false
		}
		if cached.URL != "" {
			return cached.URL, true
		}
		// Negative cache: skip online sources, but a later mpris entry may
		// still apply.
	}
	return "", true
}

// Lookup runs the full chain (blocking; call it from a goroutine). Each
// HTTP request is bounded by RequestTimeout. Results, including negative
// ones, are cached.
func (r *Resolver) Lookup(ctx context.Context, q Query) string {
	searchable := q.Searchable()
	key := q.CacheKey()
	queried, failed := false, false
	for _, name := range r.order {
		if ctx.Err() != nil {
			return ""
		}
		if name == "mpris" {
			if PublicURL(q.ArtURL) {
				return q.ArtURL
			}
			continue
		}
		if !searchable {
			continue
		}
		if e, ok := r.cache.Get(key); ok {
			if e.URL != "" {
				return e.URL
			}
			continue
		}
		queried = true
		src := r.sources[name]
		u, err := src.Lookup(ctx, q)
		if err != nil {
			if ctx.Err() != nil {
				return ""
			}
			failed = true
			r.log.Debug("album art lookup failed", "source", name, "err", err)
			continue
		}
		if u != "" && PublicURL(u) {
			r.log.Debug("album art found", "source", name, "url", u)
			if err := r.cache.Put(key, u, name, PositiveTTL); err != nil {
				r.log.Debug("writing art cache failed", "err", err)
			}
			return u
		}
	}
	if queried && ctx.Err() == nil {
		ttl := NegativeTTL
		if failed {
			ttl = ErrorNegativeTTL
		}
		if err := r.cache.Put(key, "", "", ttl); err != nil {
			r.log.Debug("writing art cache failed", "err", err)
		}
	}
	return ""
}
