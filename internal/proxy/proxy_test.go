package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOrigin is a programmable, call-counting origin transport.
type fakeOrigin struct {
	mu      sync.Mutex
	calls   int
	inms    []string // If-None-Match seen on each call
	respond func(r *http.Request) (*http.Response, error)
}

func (f *fakeOrigin) Do(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls++
	f.inms = append(f.inms, r.Header.Get("If-None-Match"))
	respond := f.respond
	f.mu.Unlock()
	return respond(r)
}

func (f *fakeOrigin) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeOrigin) inmAt(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inms[i]
}

func (f *fakeOrigin) conditionalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, inm := range f.inms {
		if inm != "" {
			n++
		}
	}
	return n
}

// makeResp builds an origin response. maxAge < 0 omits Cache-Control.
func makeResp(status int, etag, body string, maxAge int, vary string) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if etag != "" {
		h.Set("ETag", etag)
	}
	if maxAge >= 0 {
		h.Set("Cache-Control", fmt.Sprintf("max-age=%d", maxAge))
	}
	if vary != "" {
		h.Set("Vary", vary)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func notModified(etag string, maxAge int) *http.Response {
	h := http.Header{}
	if etag != "" {
		h.Set("ETag", etag)
	}
	if maxAge >= 0 {
		h.Set("Cache-Control", fmt.Sprintf("max-age=%d", maxAge))
	}
	return &http.Response{
		StatusCode: http.StatusNotModified,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

var errBoom = errors.New("origin unreachable")

func newTestProxy(origin Doer, clock Clock) *Proxy {
	return New("http://origin.test", origin, clock, 30*time.Second, 5*time.Second)
}

func doRequest(p *Proxy, method, path, lang, inm string, ctx context.Context) *httptest.ResponseRecorder {
	if ctx == nil {
		ctx = context.Background()
	}
	req := httptest.NewRequest(method, path, nil).WithContext(ctx)
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	if inm != "" {
		req.Header.Set("If-None-Match", inm)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func get(t *testing.T, p *Proxy, path, lang string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(p, http.MethodGet, path, lang, "", nil)
}

func assertCacheStatus(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := rec.Header().Get("X-Proxy-Cache"); got != want {
		t.Errorf("X-Proxy-Cache = %q, want %q (code=%d body=%q)", got, want, rec.Code, rec.Body.String())
	}
}

// blockingOrigin holds every origin call on gate until it is closed (or the
// request context times out), and signals on entered.
func blockingOrigin(gate <-chan struct{}, entered chan<- struct{}) *fakeOrigin {
	return &fakeOrigin{respond: func(r *http.Request) (*http.Response, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-gate:
			return makeResp(200, `"g1"`, `{"lang":"en"}`, 30, "Accept-Language"), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}}
}

func TestConcurrentCoalescing(t *testing.T) {
	clock := NewFakeClock(time.Unix(1_700_000_000, 0))
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	fo := blockingOrigin(gate, entered)
	p := newTestProxy(fo, clock)

	const n = 10
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recs[i] = doRequest(p, http.MethodGet, "/assets/a", "en", "", nil)
		}(i)
	}
	close(start)
	<-entered                          // the leader reached the origin
	time.Sleep(100 * time.Millisecond) // let the followers pile onto the flight
	close(gate)
	wg.Wait()

	if got := fo.callCount(); got != 1 {
		t.Fatalf("origin calls = %d, want 1", got)
	}
	for i, rec := range recs {
		if rec.Code != 200 || rec.Body.String() != `{"lang":"en"}` {
			t.Errorf("waiter %d got code=%d body=%q, want 200 shared body", i, rec.Code, rec.Body.String())
		}
	}
}

func TestConcurrentRevalidationCoalesced(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	clock := NewFakeClock(t0)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	var useGate atomic.Bool
	useGate.Store(false)
	fo := &fakeOrigin{respond: func(r *http.Request) (*http.Response, error) {
		if useGate.Load() {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-gate
			return notModified(`"r1"`, -1), nil // 304 without Cache-Control keeps old max-age
		}
		return makeResp(200, `"r1"`, "body-v1", 5, "Accept-Language"), nil
	}}
	p := newTestProxy(fo, clock)

	rec := get(t, p, "/assets/rc", "en")
	assertCacheStatus(t, rec, CacheMiss)

	clock.Set(t0.Add(6 * time.Second)) // expired
	useGate.Store(true)

	const n = 8
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			recs[i] = get(t, p, "/assets/rc", "en")
		}(i)
	}
	close(start)
	<-entered
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := fo.callCount(); got != 2 {
		t.Fatalf("origin calls = %d, want 2 (store + one shared revalidation)", got)
	}
	if got := fo.conditionalCalls(); got != 1 {
		t.Fatalf("conditional origin calls = %d, want 1", got)
	}
	if got := fo.inmAt(1); got != `"r1"` {
		t.Errorf("revalidation sent If-None-Match %q, want %q", got, `"r1"`)
	}
	for i, rec := range recs {
		if rec.Code != 200 || rec.Body.String() != "body-v1" {
			t.Errorf("waiter %d got code=%d body=%q", i, rec.Code, rec.Body.String())
		}
		assertCacheStatus(t, rec, CacheRevalidated)
	}

	// Freshness was extended by the 304: no new origin call within max-age.
	rec = get(t, p, "/assets/rc", "en")
	assertCacheStatus(t, rec, CacheHit)
	if got := fo.callCount(); got != 2 {
		t.Fatalf("origin calls after 304 = %d, want 2", got)
	}
}

func TestCancelledWaiterDoesNotAffectOthers(t *testing.T) {
	clock := NewFakeClock(time.Unix(1_700_000_000, 0))
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	fo := blockingOrigin(gate, entered)
	p := newTestProxy(fo, clock)

	leaderDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { leaderDone <- doRequest(p, http.MethodGet, "/assets/x", "en", "", nil) }()
	<-entered // leader is fetching from the origin

	ctxB, cancelB := context.WithCancel(context.Background())
	bDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { bDone <- doRequest(p, http.MethodGet, "/assets/x", "en", "", ctxB) }()
	cDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { cDone <- doRequest(p, http.MethodGet, "/assets/x", "en", "", nil) }()

	time.Sleep(100 * time.Millisecond) // B and C are parked on the flight
	cancelB()
	select {
	case rec := <-bDone:
		if rec.Code != 499 {
			t.Errorf("cancelled waiter status = %d, want 499", rec.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter did not return promptly")
	}

	// The shared fetch must still be in flight for the others.
	select {
	case <-cDone:
		t.Fatal("waiter C returned before the origin responded")
	default:
	}

	close(gate)
	leader := <-leaderDone
	c := <-cDone
	if leader.Code != 200 || c.Code != 200 {
		t.Errorf("leader=%d C=%d, want both 200", leader.Code, c.Code)
	}
	if got := fo.callCount(); got != 1 {
		t.Errorf("origin calls = %d, want 1", got)
	}
}

func TestCancelledLeaderDoesNotAffectFollowers(t *testing.T) {
	clock := NewFakeClock(time.Unix(1_700_000_000, 0))
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	fo := blockingOrigin(gate, entered)
	p := newTestProxy(fo, clock)

	ctxL, cancelL := context.WithCancel(context.Background())
	leaderDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { leaderDone <- doRequest(p, http.MethodGet, "/assets/y", "en", "", ctxL) }()
	<-entered

	fDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { fDone <- doRequest(p, http.MethodGet, "/assets/y", "en", "", nil) }()
	time.Sleep(100 * time.Millisecond)

	cancelL()                         // the leader's client goes away
	time.Sleep(50 * time.Millisecond) // the fetch must not be aborted
	select {
	case <-fDone:
		t.Fatal("follower returned before the origin responded")
	default:
	}

	close(gate)
	f := <-fDone
	if f.Code != 200 || f.Body.String() != `{"lang":"en"}` {
		t.Errorf("follower got code=%d body=%q, want 200 shared body", f.Code, f.Body.String())
	}
	<-leaderDone
	if got := fo.callCount(); got != 1 {
		t.Errorf("origin calls = %d, want 1", got)
	}
}

func TestExpiryBoundaryAnd304ExtendsFreshness(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	clock := NewFakeClock(t0)
	fo := &fakeOrigin{respond: func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("If-None-Match") == `"e1"` {
			return notModified(`"e1"`, 10), nil
		}
		return makeResp(200, `"e1"`, "v1", 10, "Accept-Language"), nil
	}}
	p := newTestProxy(fo, clock)

	rec := get(t, p, "/assets/b", "en")
	assertCacheStatus(t, rec, CacheMiss)
	if rec.Body.String() != "v1" {
		t.Fatalf("body = %q, want v1", rec.Body.String())
	}

	clock.Set(t0.Add(10*time.Second - time.Millisecond)) // 1ms before expiry
	rec = get(t, p, "/assets/b", "en")
	assertCacheStatus(t, rec, CacheHit)
	if got := fo.callCount(); got != 1 {
		t.Fatalf("origin calls = %d, want 1", got)
	}

	clock.Set(t0.Add(10 * time.Second)) // exactly at expiry -> revalidate
	rec = get(t, p, "/assets/b", "en")
	assertCacheStatus(t, rec, CacheRevalidated)
	if got := fo.callCount(); got != 2 {
		t.Fatalf("origin calls = %d, want 2", got)
	}
	if got := fo.inmAt(1); got != `"e1"` {
		t.Errorf("conditional request carried If-None-Match %q, want %q", got, `"e1"`)
	}
	if rec.Body.String() != "v1" {
		t.Errorf("revalidated body = %q, want v1", rec.Body.String())
	}

	// The 304 extended freshness by another 10s from t0+10.
	clock.Set(t0.Add(20*time.Second - time.Millisecond))
	rec = get(t, p, "/assets/b", "en")
	assertCacheStatus(t, rec, CacheHit)
	if got := fo.callCount(); got != 2 {
		t.Fatalf("origin calls = %d, want 2 (304 must extend freshness)", got)
	}

	clock.Set(t0.Add(20 * time.Second))
	rec = get(t, p, "/assets/b", "en")
	assertCacheStatus(t, rec, CacheRevalidated)
	if got := fo.callCount(); got != 3 {
		t.Fatalf("origin calls = %d, want 3", got)
	}
}

func Test304CanUpdateMaxAge(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	clock := NewFakeClock(t0)
	fo := &fakeOrigin{respond: func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("If-None-Match") == `"m1"` {
			return notModified(`"m1"`, 8), nil // origin extends max-age to 8
		}
		return makeResp(200, `"m1"`, "mv", 5, "Accept-Language"), nil
	}}
	p := newTestProxy(fo, clock)

	get(t, p, "/assets/m", "en")
	clock.Set(t0.Add(5 * time.Second))
	rec := get(t, p, "/assets/m", "en")
	assertCacheStatus(t, rec, CacheRevalidated)

	clock.Set(t0.Add(13*time.Second - time.Millisecond)) // within the new 8s window
	rec = get(t, p, "/assets/m", "en")
	assertCacheStatus(t, rec, CacheHit)
	if got := fo.callCount(); got != 2 {
		t.Fatalf("origin calls = %d, want 2", got)
	}

	clock.Set(t0.Add(13 * time.Second))
	rec = get(t, p, "/assets/m", "en")
	assertCacheStatus(t, rec, CacheRevalidated)
	if got := fo.callCount(); got != 3 {
		t.Fatalf("origin calls = %d, want 3", got)
	}
}

func TestMaxAgeZeroRevalidatesEveryTime(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	clock := NewFakeClock(t0)
	fo := &fakeOrigin{respond: func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("If-None-Match") == `"z0"` {
			return notModified(`"z0"`, 0), nil
		}
		return makeResp(200, `"z0"`, "zero", 0, "Accept-Language"), nil
	}}
	p := newTestProxy(fo, clock)

	rec := get(t, p, "/assets/z", "en")
	assertCacheStatus(t, rec, CacheMiss)
	rec = get(t, p, "/assets/z", "en")
	assertCacheStatus(t, rec, CacheRevalidated)
	rec = get(t, p, "/assets/z", "en")
	assertCacheStatus(t, rec, CacheRevalidated)
	if got := fo.callCount(); got != 3 {
		t.Fatalf("origin calls = %d, want 3 (max-age=0 is stale immediately)", got)
	}
	if rec.Body.String() != "zero" || rec.Code != 200 {
		t.Errorf("got code=%d body=%q, want 200 zero", rec.Code, rec.Body.String())
	}
}

func TestCrossLanguageIsolation(t *testing.T) {
	clock := NewFakeClock(time.Unix(1_700_000_000, 0))
	fo := &fakeOrigin{respond: func(r *http.Request) (*http.Response, error) {
		lang := "en"
		if strings.HasPrefix(strings.ToLower(r.Header.Get("Accept-Language")), "zh") {
			lang = "zh"
		}
		return makeResp(200, `"v1-`+lang+`"`, "body-"+lang, 30, "Accept-Language"), nil
	}}
	p := newTestProxy(fo, clock)

	rec := get(t, p, "/assets/a", "en")
	assertCacheStatus(t, rec, CacheMiss)
	if rec.Body.String() != "body-en" {
		t.Fatalf("en body = %q", rec.Body.String())
	}
	if !strings.EqualFold(rec.Header().Get("Vary"), "Accept-Language") {
		t.Errorf("Vary = %q, want Accept-Language", rec.Header().Get("Vary"))
	}

	rec = get(t, p, "/assets/a", "en")
	assertCacheStatus(t, rec, CacheHit)
	if got := fo.callCount(); got != 1 {
		t.Fatalf("origin calls = %d, want 1", got)
	}

	// A zh request must not be served the cached en variant.
	rec = get(t, p, "/assets/a", "zh")
	assertCacheStatus(t, rec, CacheMiss)
	if rec.Body.String() != "body-zh" {
		t.Fatalf("zh body = %q, want body-zh (cross-language leak!)", rec.Body.String())
	}
	if got := fo.callCount(); got != 2 {
		t.Fatalf("origin calls = %d, want 2", got)
	}

	rec = get(t, p, "/assets/a", "zh")
	assertCacheStatus(t, rec, CacheHit)
	rec = get(t, p, "/assets/a", "en")
	assertCacheStatus(t, rec, CacheHit)
	if got := fo.callCount(); got != 2 {
		t.Fatalf("origin calls = %d, want 2", got)
	}

	// Client If-None-Match only yields 304 against the correct variant.
	rec = doRequest(p, http.MethodGet, "/assets/a", "en", `"v1-en"`, nil)
	if rec.Code != http.StatusNotModified {
		t.Errorf("en + en ETag = %d, want 304", rec.Code)
	}
	rec = doRequest(p, http.MethodGet, "/assets/a", "en", `"v1-zh"`, nil)
	if rec.Code != 200 || rec.Body.String() != "body-en" {
		t.Errorf("en + zh ETag = %d %q, want 200 body-en (wrong variant must not 304)", rec.Code, rec.Body.String())
	}
	rec = doRequest(p, http.MethodGet, "/assets/a", "zh", `"v1-zh"`, nil)
	if rec.Code != http.StatusNotModified {
		t.Errorf("zh + zh ETag = %d, want 304", rec.Code)
	}
	rec = doRequest(p, http.MethodGet, "/assets/a", "zh", `"v1-en"`, nil)
	if rec.Code != 200 || rec.Body.String() != "body-zh" {
		t.Errorf("zh + en ETag = %d %q, want 200 body-zh", rec.Code, rec.Body.String())
	}
	if got := fo.callCount(); got != 2 {
		t.Fatalf("origin calls = %d, want 2 (conditionals served from cache)", got)
	}
}

func TestStaleServedOnOriginError(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	clock := NewFakeClock(t0)
	var mode atomic.Int32 // 0=ok, 1=http 500, 2=transport error
	fo := &fakeOrigin{respond: func(r *http.Request) (*http.Response, error) {
		switch mode.Load() {
		case 1:
			return makeResp(500, "", "server error", -1, ""), nil
		case 2:
			return nil, errBoom
		default:
			return makeResp(200, `"s1"`, "stale-body", 5, "Accept-Language"), nil
		}
	}}
	p := newTestProxy(fo, clock)

	rec := get(t, p, "/assets/s", "en")
	assertCacheStatus(t, rec, CacheMiss)

	mode.Store(1)
	clock.Set(t0.Add(6 * time.Second)) // expired by 1s
	rec = get(t, p, "/assets/s", "en")
	if rec.Code != 200 || rec.Body.String() != "stale-body" {
		t.Fatalf("stale on 500: code=%d body=%q", rec.Code, rec.Body.String())
	}
	assertCacheStatus(t, rec, CacheStale)
	if rec.Header().Get("X-Proxy-Stale") != "true" {
		t.Errorf("X-Proxy-Stale = %q, want true", rec.Header().Get("X-Proxy-Stale"))
	}
	if !strings.Contains(rec.Header().Get("Warning"), "110") {
		t.Errorf("Warning = %q, want it to contain 110", rec.Header().Get("Warning"))
	}

	// Transport errors also trigger stale-if-error.
	mode.Store(2)
	clock.Set(t0.Add(35*time.Second - time.Millisecond)) // last instant of the 30s stale window
	rec = get(t, p, "/assets/s", "en")
	assertCacheStatus(t, rec, CacheStale)
	if rec.Code != 200 {
		t.Errorf("stale at window edge: code=%d, want 200", rec.Code)
	}

	// Beyond max-age(5s)+stale(30s) the entry must not be served.
	clock.Set(t0.Add(35 * time.Second))
	rec = get(t, p, "/assets/s", "en")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("beyond stale window: code=%d, want 502", rec.Code)
	}

	// Origin recovers: fresh content again, no stale marker.
	mode.Store(0)
	rec = get(t, p, "/assets/s", "en")
	if rec.Code != 200 || rec.Header().Get("X-Proxy-Stale") != "" {
		t.Errorf("after recovery: code=%d X-Proxy-Stale=%q", rec.Code, rec.Header().Get("X-Proxy-Stale"))
	}
}

func TestOriginErrorWithoutCacheGives502(t *testing.T) {
	clock := NewFakeClock(time.Unix(1_700_000_000, 0))
	fo := &fakeOrigin{respond: func(r *http.Request) (*http.Response, error) {
		return makeResp(500, "", "server error", -1, ""), nil
	}}
	p := newTestProxy(fo, clock)
	if rec := get(t, p, "/assets/e", "en"); rec.Code != http.StatusBadGateway {
		t.Errorf("origin 500 with empty cache: code=%d, want 502", rec.Code)
	}

	fo.respond = func(r *http.Request) (*http.Response, error) { return nil, errBoom }
	p = newTestProxy(fo, clock)
	if rec := get(t, p, "/assets/e", "en"); rec.Code != http.StatusBadGateway {
		t.Errorf("origin unreachable with empty cache: code=%d, want 502", rec.Code)
	}
}

func TestCacheabilityMatrix(t *testing.T) {
	cases := []struct {
		name   string
		status int
		etag   string
		cc     string // raw Cache-Control; "" means the header is absent
		vary   string
		cached bool
	}{
		{"basic", 200, `"e"`, "max-age=10", "Accept-Language", true},
		{"max-age 60 allowed", 200, `"e"`, "max-age=60", "", true},
		{"max-age 61 rejected", 200, `"e"`, "max-age=61", "", false},
		{"no max-age", 200, `"e"`, "public", "", false},
		{"no cache-control", 200, `"e"`, "", "", false},
		{"no-store", 200, `"e"`, "no-store, max-age=10", "", false},
		{"no-cache", 200, `"e"`, "no-cache, max-age=10", "", false},
		{"garbage max-age", 200, `"e"`, "max-age=abc", "", false},
		{"missing etag", 200, "", "max-age=10", "", false},
		{"status 201", 201, `"e"`, "max-age=10", "", false},
		{"vary accept-encoding", 200, `"e"`, "max-age=10", "Accept-Encoding", false},
		{"vary multiple fields", 200, `"e"`, "max-age=10", "Accept-Language, Accept-Encoding", false},
		{"vary star", 200, `"e"`, "max-age=10", "*", false},
		{"vary case-insensitive", 200, `"e"`, "max-age=10", "accept-language", true},
		{"quoted max-age", 200, `"e"`, `max-age="10"`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := NewFakeClock(time.Unix(1_700_000_000, 0))
			fo := &fakeOrigin{respond: func(r *http.Request) (*http.Response, error) {
				maxAge := -1
				if tc.cc != "" {
					maxAge = -2 // marker: set raw header below
				}
				resp := makeResp(tc.status, tc.etag, "body", maxAge, tc.vary)
				if maxAge == -2 {
					resp.Header.Set("Cache-Control", tc.cc)
				}
				return resp, nil
			}}
			p := newTestProxy(fo, clock)

			rec1 := get(t, p, "/assets/mx", "en")
			if rec1.Code != tc.status {
				t.Fatalf("first response code = %d, want %d", rec1.Code, tc.status)
			}
			rec2 := get(t, p, "/assets/mx", "en")
			if tc.cached {
				if got := fo.callCount(); got != 1 {
					t.Errorf("origin calls = %d, want 1 (response should be cached)", got)
				}
				assertCacheStatus(t, rec2, CacheHit)
			} else {
				if got := fo.callCount(); got != 2 {
					t.Errorf("origin calls = %d, want 2 (response must not be cached)", got)
				}
				assertCacheStatus(t, rec2, CachePass)
			}
		})
	}
}

func TestUncacheableStatusPassesThrough(t *testing.T) {
	clock := NewFakeClock(time.Unix(1_700_000_000, 0))
	fo := &fakeOrigin{respond: func(r *http.Request) (*http.Response, error) {
		return makeResp(404, "", "not found", -1, ""), nil
	}}
	p := newTestProxy(fo, clock)
	rec := get(t, p, "/assets/ghost", "en")
	if rec.Code != 404 {
		t.Errorf("code = %d, want 404 passthrough", rec.Code)
	}
	assertCacheStatus(t, rec, CachePass)
	get(t, p, "/assets/ghost", "en")
	if got := fo.callCount(); got != 2 {
		t.Errorf("origin calls = %d, want 2 (404 not cached)", got)
	}
}

func TestNormalizeVariant(t *testing.T) {
	cases := []struct {
		header string
		want   string
		ok     bool
	}{
		{"zh", "zh", true},
		{"zh-CN,zh;q=0.9", "zh", true},
		{"zh-Hans-CN", "zh", true},
		{"en", "en", true},
		{"en-US,en;q=0.5", "en", true},
		{"en;q=0.5,zh;q=0.9", "zh", true},
		{"fr", "", false},
		{"fr,en;q=0.9", "", false}, // fr wins on q=1
		{"de-DE,de;q=0.8,en;q=0.5", "", false},
		{"", "", false},
		{"*", "", false},
		{"en;q=0", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.header, func(t *testing.T) {
			got, ok := normalizeVariant(tc.header)
			if got != tc.want || ok != tc.ok {
				t.Errorf("normalizeVariant(%q) = %q,%v, want %q,%v", tc.header, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestUnsupportedVariantBypassesCache(t *testing.T) {
	clock := NewFakeClock(time.Unix(1_700_000_000, 0))
	fo := &fakeOrigin{respond: func(r *http.Request) (*http.Response, error) {
		return makeResp(200, `"u1"`, "body", 30, "Accept-Language"), nil
	}}
	p := newTestProxy(fo, clock)

	rec := doRequest(p, http.MethodGet, "/assets/u", "fr", "", nil)
	if rec.Code != 200 {
		t.Fatalf("fr request code = %d", rec.Code)
	}
	assertCacheStatus(t, rec, CachePass)
	rec = doRequest(p, http.MethodGet, "/assets/u", "fr", "", nil)
	assertCacheStatus(t, rec, CachePass)
	rec = doRequest(p, http.MethodGet, "/assets/u", "", "", nil)
	assertCacheStatus(t, rec, CachePass)
	if got := fo.callCount(); got != 3 {
		t.Errorf("origin calls = %d, want 3 (unsupported variants never cached)", got)
	}
}

func TestIfNoneMatchForms(t *testing.T) {
	clock := NewFakeClock(time.Unix(1_700_000_000, 0))
	fo := &fakeOrigin{respond: func(r *http.Request) (*http.Response, error) {
		return makeResp(200, `"e1"`, "body", 30, "Accept-Language"), nil
	}}
	p := newTestProxy(fo, clock)
	get(t, p, "/assets/inm", "en")

	for _, inm := range []string{`*`, `"other", "e1"`, `W/"e1"`} {
		rec := doRequest(p, http.MethodGet, "/assets/inm", "en", inm, nil)
		if rec.Code != http.StatusNotModified {
			t.Errorf("If-None-Match %q: code = %d, want 304", inm, rec.Code)
		}
	}
	rec := doRequest(p, http.MethodGet, "/assets/inm", "en", `"nope"`, nil)
	if rec.Code != 200 {
		t.Errorf("If-None-Match mismatch: code = %d, want 200", rec.Code)
	}
	if got := fo.callCount(); got != 1 {
		t.Errorf("origin calls = %d, want 1", got)
	}
}

func TestRouting(t *testing.T) {
	clock := NewFakeClock(time.Unix(1_700_000_000, 0))
	fo := &fakeOrigin{respond: func(r *http.Request) (*http.Response, error) {
		return makeResp(200, `"e"`, "b", 10, "Accept-Language"), nil
	}}
	p := newTestProxy(fo, clock)

	if rec := doRequest(p, http.MethodPost, "/assets/a", "en", "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /assets/a = %d, want 405", rec.Code)
	}
	for _, path := range []string{"/", "/assets", "/assets/", "/assets/a/b", "/other"} {
		if rec := doRequest(p, http.MethodGet, path, "en", "", nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
	if got := fo.callCount(); got != 0 {
		t.Errorf("origin calls = %d, want 0 for rejected requests", got)
	}
}
