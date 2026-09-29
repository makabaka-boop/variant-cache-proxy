package proxy

import (
	"net/http"
	"sync"
	"time"
)

// Entry is a cached origin response for one asset variant.
type Entry struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	ETag       string
	MaxAge     time.Duration
	StoredAt   time.Time
}

// freshUntil is the instant at which the entry stops being fresh.
func (e *Entry) freshUntil() time.Time { return e.StoredAt.Add(e.MaxAge) }

func (e *Entry) clone() *Entry {
	ne := *e
	ne.Header = e.Header.Clone()
	ne.Body = append([]byte(nil), e.Body...)
	return &ne
}

// Cache stores entries keyed by variant|id. Entries are cloned in and out so
// callers never share mutable state.
type Cache struct {
	mu sync.RWMutex
	m  map[string]*Entry
}

// NewCache returns an empty Cache.
func NewCache() *Cache { return &Cache{m: make(map[string]*Entry)} }

// Get returns a copy of the entry for key.
func (c *Cache) Get(key string) (*Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.m[key]
	if !ok {
		return nil, false
	}
	return e.clone(), true
}

// Set stores a copy of e under key.
func (c *Cache) Set(key string, e *Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = e.clone()
}
