package art

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"tunecord/internal/config"
)

// Cache TTLs.
const (
	PositiveTTL      = 30 * 24 * time.Hour
	NegativeTTL      = 24 * time.Hour
	ErrorNegativeTTL = 15 * time.Minute // lookups that failed due to network errors
	maxEntries       = 5000
)

// Entry is a cached lookup result. URL == "" is a negative result.
type Entry struct {
	URL     string    `json:"url,omitempty"`
	Source  string    `json:"source,omitempty"`
	Stored  time.Time `json:"stored"`
	Expires time.Time `json:"expires"`
}

// Cache is an in-memory cache persisted to a JSON file.
type Cache struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	entries map[string]Entry
}

type cacheFile struct {
	Version int              `json:"version"`
	Entries map[string]Entry `json:"entries"`
}

// OpenCache loads the cache at path (a missing or corrupt file starts empty).
func OpenCache(path string) *Cache {
	c := &Cache{path: path, now: time.Now, entries: map[string]Entry{}}
	if path == "" {
		return c
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var f cacheFile
	if json.Unmarshal(data, &f) == nil && f.Entries != nil {
		c.entries = f.Entries
	}
	return c
}

// DefaultCachePath returns ~/.cache/tunecord/art.json.
func DefaultCachePath() string { return filepath.Join(config.CacheDir(), "art.json") }

// Get returns a non-expired entry.
func (c *Cache) Get(key string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return Entry{}, false
	}
	if c.now().After(e.Expires) {
		delete(c.entries, key)
		return Entry{}, false
	}
	return e, true
}

// Put stores an entry with the given TTL and persists the cache.
func (c *Cache) Put(key, url, source string, ttl time.Duration) error {
	now := c.now()
	c.mu.Lock()
	c.entries[key] = Entry{URL: url, Source: source, Stored: now, Expires: now.Add(ttl)}
	c.pruneLocked(now)
	data, err := json.MarshalIndent(cacheFile{Version: 1, Entries: c.entries}, "", " ")
	c.mu.Unlock()
	if err != nil || c.path == "" {
		return err
	}
	return config.WriteFileAtomic(c.path, data, 0o600)
}

func (c *Cache) pruneLocked(now time.Time) {
	for k, e := range c.entries {
		if now.After(e.Expires) {
			delete(c.entries, k)
		}
	}
	for len(c.entries) > maxEntries {
		var oldestKey string
		var oldest time.Time
		for k, e := range c.entries {
			if oldestKey == "" || e.Stored.Before(oldest) {
				oldestKey, oldest = k, e.Stored
			}
		}
		delete(c.entries, oldestKey)
	}
}

// Len returns the number of entries.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
