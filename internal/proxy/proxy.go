// Package proxy implements a caching HTTP proxy for GET /assets/{id} with
// per-language variants, conditional revalidation, request coalescing and
// stale-if-error serving.
package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// X-Proxy-Cache values.
const (
	CacheHit         = "HIT"
	CacheMiss        = "MISS"
	CacheRevalidated = "REVALIDATED"
	CacheStale       = "STALE"
	CachePass        = "PASS"
)

const maxOriginBody = 32 << 20

// Doer is the origin transport; *http.Client satisfies it.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Proxy is the caching reverse proxy. Use New to construct it.
type Proxy struct {
	origin       string
	client       Doer
	clock        Clock
	staleWindow  time.Duration
	fetchTimeout time.Duration

	cache *Cache
	group flightGroup
}

// New builds a Proxy. origin is the origin base URL (e.g.
// http://origin:9000). client and clock may be nil for production defaults.
// staleWindow is how long past expiry a stale entry may be served when the
// origin fails (default 30s); fetchTimeout bounds one origin fetch (default
// 10s).
func New(origin string, client Doer, clock Clock, staleWindow, fetchTimeout time.Duration) *Proxy {
	if client == nil {
		client = &http.Client{}
	}
	if clock == nil {
		clock = RealClock()
	}
	if staleWindow <= 0 {
		staleWindow = 30 * time.Second
	}
	if fetchTimeout <= 0 {
		fetchTimeout = 10 * time.Second
	}
	return &Proxy{
		origin:       strings.TrimRight(origin, "/"),
		client:       client,
		clock:        clock,
		staleWindow:  staleWindow,
		fetchTimeout: fetchTimeout,
		cache:        NewCache(),
	}
}

// ServeHTTP handles GET /assets/{id}; everything else is 404/405.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, ok := parseAssetPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	variant, ok := normalizeVariant(r.Header.Get("Accept-Language"))
	if !ok {
		// Unsupported language: proxy directly, never cache.
		p.forward(w, r)
		return
	}
	key := variant + "|" + id
	if e, hit := p.cache.Get(key); hit && p.clock.Now().Before(e.freshUntil()) {
		p.serveEntry(w, r, e, CacheHit, false)
		return
	}
	res := p.group.do(key, r.Context(), func() fetchResult {
		return p.revalidate(r, key)
	})
	switch {
	case res.cancelled:
		// The client went away; nothing useful can be sent. 499 mirrors
		// nginx's client-closed-request code and is observable in tests.
		w.WriteHeader(499)
	case res.direct != nil:
		writeDirect(w, res.direct)
	case res.entry != nil:
		p.serveEntry(w, r, res.entry, res.status, res.stale)
	default:
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}
}

func parseAssetPath(path string) (string, bool) {
	if !strings.HasPrefix(path, "/assets/") {
		return "", false
	}
	id := strings.TrimPrefix(path, "/assets/")
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// revalidate performs the single shared origin fetch for key.
func (p *Proxy) revalidate(clientReq *http.Request, key string) fetchResult {
	existing, hasExisting := p.cache.Get(key)
	// Recheck: a previous flight may have refreshed the entry while this
	// request waited to become the leader.
	if hasExisting && p.clock.Now().Before(existing.freshUntil()) {
		return fetchResult{entry: existing, status: CacheHit}
	}

	inm := ""
	if hasExisting {
		inm = existing.ETag
	}
	resp, body, err := p.fetchFromOrigin(clientReq, inm)
	if err != nil {
		return p.staleOrError(existing, hasExisting)
	}

	if resp.StatusCode == http.StatusNotModified && hasExisting {
		// 304 extends the freshness of the stored response: merge the new
		// headers and restart the freshness clock.
		ne := existing.clone()
		ne.StoredAt = p.clock.Now()
		for k, vs := range resp.Header {
			ne.Header[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
		}
		ne.ETag = ne.Header.Get("ETag")
		if ma, ok := maxAgeDirective(resp.Header); ok {
			ne.MaxAge = ma
		}
		p.cache.Set(key, ne)
		return fetchResult{entry: ne, status: CacheRevalidated}
	}
	if ma, ok := cacheableMaxAge(resp.StatusCode, resp.Header); ok {
		e := &Entry{
			StatusCode: resp.StatusCode,
			Header:     resp.Header.Clone(),
			Body:       body,
			ETag:       resp.Header.Get("ETag"),
			MaxAge:     ma,
			StoredAt:   p.clock.Now(),
		}
		p.cache.Set(key, e)
		return fetchResult{entry: e, status: CacheMiss}
	}
	if resp.StatusCode >= 500 {
		return p.staleOrError(existing, hasExisting)
	}
	// Anything else (404, uncacheable 200, ...) is forwarded as-is.
	return fetchResult{direct: &directResponse{status: resp.StatusCode, header: resp.Header, body: body}}
}

// staleOrError serves the expired entry while it is within the stale
// window; otherwise the origin failure is surfaced as an empty result
// (which the handler turns into 502).
func (p *Proxy) staleOrError(e *Entry, ok bool) fetchResult {
	if ok && p.clock.Now().Before(e.freshUntil().Add(p.staleWindow)) {
		return fetchResult{entry: e, status: CacheStale, stale: true}
	}
	return fetchResult{}
}

// fetchFromOrigin issues the origin request. It is deliberately detached
// from the client request context: one client cancelling must not abort a
// fetch other waiters depend on.
func (p *Proxy) fetchFromOrigin(clientReq *http.Request, ifNoneMatch string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, p.origin+clientReq.URL.Path, nil)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.fetchTimeout)
	defer cancel()
	req = req.WithContext(ctx)
	if al := clientReq.Header.Get("Accept-Language"); al != "" {
		req.Header.Set("Accept-Language", al)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOriginBody+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > maxOriginBody {
		return nil, nil, errors.New("origin response too large")
	}
	return resp, body, nil
}

// forward proxies a request without caching (unsupported language variant).
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, p.origin+r.URL.Path, nil)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	for _, k := range []string{"Accept-Language", "If-None-Match"} {
		if v := r.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := p.client.Do(req)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOriginBody+1))
	if err != nil || len(body) > maxOriginBody {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	writeDirect(w, &directResponse{status: resp.StatusCode, header: resp.Header, body: body})
}

// skippedHeaders are never copied from stored/origin responses: hop-by-hop
// headers plus fields the proxy recomputes.
var skippedHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Content-Length":      true,
	"Date":                true,
}

func skipHeader(k string) bool { return skippedHeaders[http.CanonicalHeaderKey(k)] }

func writeDirect(w http.ResponseWriter, d *directResponse) {
	h := w.Header()
	for k, vs := range d.header {
		if skipHeader(k) {
			continue
		}
		h[k] = append([]string(nil), vs...)
	}
	h.Set("X-Proxy-Cache", CachePass)
	h.Set("Content-Length", strconv.Itoa(len(d.body)))
	w.WriteHeader(d.status)
	_, _ = w.Write(d.body)
}

func (p *Proxy) serveEntry(w http.ResponseWriter, r *http.Request, e *Entry, status string, stale bool) {
	h := w.Header()
	for k, vs := range e.Header {
		if skipHeader(k) {
			continue
		}
		h[k] = append([]string(nil), vs...)
	}
	h.Set("X-Proxy-Cache", status)
	if stale {
		h.Set("Warning", `110 - "Response is stale"`)
		h.Set("X-Proxy-Stale", "true")
	}
	age := p.clock.Now().Sub(e.StoredAt).Seconds()
	if age < 0 {
		age = 0
	}
	h.Set("Age", strconv.Itoa(int(age)))
	// The client's If-None-Match is evaluated against this variant's entry
	// only, so a 304 can never be produced using another language's ETag.
	if matchIfNoneMatch(r.Header.Get("If-None-Match"), e.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(e.Body)))
	w.WriteHeader(e.StatusCode)
	_, _ = w.Write(e.Body)
}

// matchIfNoneMatch implements If-None-Match with weak comparison, supporting
// lists and "*".
func matchIfNoneMatch(inm, etag string) bool {
	if inm == "" || etag == "" {
		return false
	}
	for _, t := range strings.Split(inm, ",") {
		t = strings.TrimSpace(t)
		if t == "*" {
			return true
		}
		if strings.TrimPrefix(t, "W/") == strings.TrimPrefix(etag, "W/") {
			return true
		}
	}
	return false
}
