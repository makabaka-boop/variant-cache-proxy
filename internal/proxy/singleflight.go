package proxy

import (
	"context"
	"net/http"
	"sync"
)

// fetchResult is the outcome of a (possibly shared) origin fetch.
type fetchResult struct {
	entry  *Entry // serve from this cached entry
	status string // X-Proxy-Cache value when entry != nil
	stale  bool   // entry is stale (served because the origin failed)

	direct *directResponse // uncacheable response to forward as-is

	cancelled bool // this waiter gave up before the fetch completed
}

// directResponse is a non-cacheable origin response forwarded verbatim.
type directResponse struct {
	status int
	header http.Header
	body   []byte
}

type call struct {
	done chan struct{}
	res  fetchResult
}

// flightGroup coalesces concurrent fetches for the same key. The fetch runs
// detached from any single request, so one waiter cancelling never
// interrupts the fetch or the other waiters.
type flightGroup struct {
	mu sync.Mutex
	m  map[string]*call
}

// do runs fn for key, sharing the result with concurrent callers. Followers
// wait on ctx; if ctx is done first they get a cancelled result while the
// shared fetch keeps running for the remaining waiters.
func (g *flightGroup) do(key string, ctx context.Context, fn func() fetchResult) fetchResult {
	g.mu.Lock()
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		select {
		case <-c.done:
			return c.res
		case <-ctx.Done():
			return fetchResult{cancelled: true}
		}
	}
	c := &call{done: make(chan struct{})}
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	g.m[key] = c
	g.mu.Unlock()

	c.res = fn()
	close(c.done)

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	return c.res
}
