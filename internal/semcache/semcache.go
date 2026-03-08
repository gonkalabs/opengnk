// Package semcache implements an L1 exact-match cache for opengnk.
//
// Cache key: SHA256 of the canonical JSON encoding of the messages array
// (identical to Gonka DAPI's PromptHash — see quality.CanonicalPromptHash).
//
// On HIT:  return cached response + set X-Cache: HIT header.
// On MISS: forward to DAPI, store response, set X-Cache: MISS.
//
// This enables quality.Middleware to measure L6 (cache reuse rate) through
// X-Cache headers — the same axis tracked by GiP #860.
//
// Storage: data/semcache.json (JSON, rotated at maxEntries).
// All operations are protected by sync.RWMutex — safe for concurrent requests.
package semcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"
)

const (
	DefaultMaxEntries = 500
	DefaultTTL        = 24 * time.Hour
)

// entry is one cached response.
type entry struct {
	Key       string          `json:"key"`
	Response  json.RawMessage `json:"response"`
	CreatedAt time.Time       `json:"created_at"`
}

// Cache is the L1 exact-match response cache.
type Cache struct {
	mu         sync.RWMutex
	entries    map[string]*entry
	order      []string // insertion order for eviction
	path       string
	maxEntries int
	ttl        time.Duration
}

// New creates a Cache and loads any persisted entries from path.
func New(path string, maxEntries int) *Cache {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	c := &Cache{
		entries:    make(map[string]*entry),
		path:       path,
		maxEntries: maxEntries,
		ttl:        DefaultTTL,
	}
	if err := c.load(); err != nil && !os.IsNotExist(err) {
		slog.Warn("semcache: load failed", "err", err)
	}
	return c
}

// Lookup returns the cached response for the given messages, or nil on miss.
func (c *Cache) Lookup(messagesJSON []byte) json.RawMessage {
	key := hashMessages(messagesJSON)
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return nil
	}
	if time.Since(e.CreatedAt) > c.ttl {
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
		return nil
	}
	return e.Response
}

// Store saves a response in the cache.
func (c *Cache) Store(messagesJSON, responseJSON []byte) {
	key := hashMessages(messagesJSON)
	e := &entry{Key: key, Response: json.RawMessage(responseJSON), CreatedAt: time.Now()}
	c.mu.Lock()
	if _, exists := c.entries[key]; !exists {
		c.order = append(c.order, key)
	}
	c.entries[key] = e
	c.evict()
	c.mu.Unlock()
	go c.persist() // async — don't block the response path
}

// ─── internal ─────────────────────────────────────────────────────────────────

func hashMessages(messagesJSON []byte) string {
	h := sha256.Sum256(messagesJSON)
	return hex.EncodeToString(h[:])
}

// evict removes the oldest entries to stay within maxEntries.
// Must be called with c.mu held (write).
func (c *Cache) evict() {
	for len(c.entries) > c.maxEntries && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
}

func (c *Cache) persist() {
	c.mu.RLock()
	entries := make([]*entry, 0, len(c.entries))
	for _, e := range c.entries {
		entries = append(entries, e)
	}
	c.mu.RUnlock()

	data, err := json.Marshal(entries)
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, c.path)
}

func (c *Cache) load() error {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return err
	}
	var entries []*entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}
	now := time.Now()
	c.mu.Lock()
	for _, e := range entries {
		if now.Sub(e.CreatedAt) <= c.ttl {
			c.entries[e.Key] = e
			c.order = append(c.order, e.Key)
		}
	}
	c.mu.Unlock()
	slog.Info("semcache: loaded", "entries", len(c.entries))
	return nil
}
