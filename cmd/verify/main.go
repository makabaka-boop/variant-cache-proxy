// Command verify runs the end-to-end acceptance suite against a live
// proxy + origin pair and exits non-zero if any check fails.
//
// Environment:
//
//	PROXY_URL   proxy base URL (default http://localhost:8080)
//	ORIGIN_URL  origin base URL (default http://localhost:9000)
//
// The suite assumes the proxy runs with STALE_IF_ERROR=3s (as configured in
// docker-compose.yml) so the stale-window checks complete quickly.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	proxyBase  = envOr("PROXY_URL", "http://localhost:8080")
	originBase = envOr("ORIGIN_URL", "http://localhost:9000")
	httpc      = &http.Client{Timeout: 15 * time.Second}
	failures   int
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func check(name string, ok bool, detail ...any) {
	if ok {
		fmt.Printf("PASS  %s\n", name)
		return
	}
	failures++
	fmt.Printf("FAIL  %s  %s\n", name, fmt.Sprint(detail...))
}

func must(err error) {
	if err != nil {
		failures++
		fmt.Println("FAIL  setup:", err)
	}
}

type resp struct {
	code int
	hdr  http.Header
	body string
}

func getAsset(base, path, lang, inm string) (resp, error) {
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return resp{}, err
	}
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	if inm != "" {
		req.Header.Set("If-None-Match", inm)
	}
	r, err := httpc.Do(req)
	if err != nil {
		return resp{}, err
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{r.StatusCode, r.Header, string(b)}, nil
}

func postConfig(fields map[string]any) error {
	buf, _ := json.Marshal(fields)
	r, err := httpc.Post(originBase+"/control/config", "application/json", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer r.Body.Close()
	_, _ = io.Copy(io.Discard, r.Body)
	if r.StatusCode != http.StatusOK {
		return fmt.Errorf("config status %d", r.StatusCode)
	}
	return nil
}

func resetOrigin() error {
	r, err := httpc.Post(originBase+"/control/reset", "application/json", nil)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	_, _ = io.Copy(io.Discard, r.Body)
	return nil
}

type stats struct {
	Hits            int            `json:"hits"`
	ConditionalHits int            `json:"conditionalHits"`
	Paths           map[string]int `json:"paths"`
}

func getStats() (stats, error) {
	var s stats
	r, err := httpc.Get(originBase + "/control/stats")
	if err != nil {
		return s, err
	}
	defer r.Body.Close()
	return s, json.NewDecoder(r.Body).Decode(&s)
}

func waitReady(name string, probe func() bool) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		if probe() {
			fmt.Printf("ready: %s\n", name)
			return
		}
		if time.Now().After(deadline) {
			fmt.Printf("FAIL  %s not ready\n", name)
			os.Exit(1)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func main() {
	fmt.Println("== verify: waiting for services ==")
	waitReady("origin", func() bool {
		r, err := httpc.Get(originBase + "/control/stats")
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
		return r.StatusCode == http.StatusOK
	})
	waitReady("proxy", func() bool {
		r, err := getAsset(proxyBase, "/assets/__ready__", "en", "")
		return err == nil && r.code == http.StatusOK
	})

	phaseRouting()
	phaseCachingAndIsolation()
	phaseCoalescing()
	phaseRevalidation()
	phaseStale()
	phaseNonCacheable()
	phaseUnsupportedLanguage()

	fmt.Println()
	if failures > 0 {
		fmt.Printf("VERIFY FAILED: %d check(s) failed\n", failures)
		os.Exit(1)
	}
	fmt.Println("VERIFY OK: all checks passed")
}

func phaseRouting() {
	fmt.Println("== routing ==")
	r, err := getAsset(proxyBase, "/nope", "en", "")
	check("unknown path -> 404", err == nil && r.code == http.StatusNotFound, r.code, err)

	req, _ := http.NewRequest(http.MethodPost, proxyBase+"/assets/route", nil)
	resp, err := httpc.Do(req)
	code := 0
	if err == nil {
		code = resp.StatusCode
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	check("POST /assets/route -> 405", err == nil && code == http.StatusMethodNotAllowed, code, err)
}

func phaseCachingAndIsolation() {
	fmt.Println("== caching & cross-language isolation ==")
	must(resetOrigin())
	must(postConfig(map[string]any{
		"maxAge": 30, "mode": "ok", "version": 1,
		"vary": "accept-language", "delayMs": 0, "noETag": false,
	}))

	r1, err := getAsset(proxyBase, "/assets/iso", "en", "")
	check("first en request -> 200 MISS", err == nil && r1.code == 200 &&
		r1.hdr.Get("X-Proxy-Cache") == "MISS", r1.code, r1.hdr.Get("X-Proxy-Cache"), err)
	check("en body has lang=en", strings.Contains(r1.body, `"lang":"en"`), r1.body)
	enETag := r1.hdr.Get("ETag")
	check("en ETag present", enETag != "")
	check("Vary: Accept-Language echoed", strings.EqualFold(r1.hdr.Get("Vary"), "Accept-Language"),
		r1.hdr.Get("Vary"))

	r2, _ := getAsset(proxyBase, "/assets/iso", "en", "")
	check("second en request -> HIT", r2.code == 200 && r2.hdr.Get("X-Proxy-Cache") == "HIT",
		r2.code, r2.hdr.Get("X-Proxy-Cache"))

	r3, _ := getAsset(proxyBase, "/assets/iso", "zh", "")
	check("zh request -> MISS with zh body (no cross-language serving)",
		r3.code == 200 && r3.hdr.Get("X-Proxy-Cache") == "MISS" && strings.Contains(r3.body, `"lang":"zh"`),
		r3.code, r3.hdr.Get("X-Proxy-Cache"), r3.body)
	zhETag := r3.hdr.Get("ETag")
	check("zh ETag differs from en ETag", zhETag != "" && zhETag != enETag, enETag, zhETag)

	r4, _ := getAsset(proxyBase, "/assets/iso", "zh", "")
	check("second zh request -> HIT", r4.code == 200 && r4.hdr.Get("X-Proxy-Cache") == "HIT",
		r4.code, r4.hdr.Get("X-Proxy-Cache"))

	s, err := getStats()
	check("origin saw exactly 2 requests for /assets/iso",
		err == nil && s.Paths["/assets/iso"] == 2, s.Paths["/assets/iso"], err)

	r5, _ := getAsset(proxyBase, "/assets/iso", "en", enETag)
	check("en If-None-Match with en ETag -> 304", r5.code == http.StatusNotModified, r5.code)
	r6, _ := getAsset(proxyBase, "/assets/iso", "en", zhETag)
	check("en If-None-Match with zh ETag -> 200 (wrong variant never 304)",
		r6.code == 200 && strings.Contains(r6.body, `"lang":"en"`), r6.code, r6.body)
	r7, _ := getAsset(proxyBase, "/assets/iso", "zh", zhETag)
	check("zh If-None-Match with zh ETag -> 304", r7.code == http.StatusNotModified, r7.code)
	r8, _ := getAsset(proxyBase, "/assets/iso", "zh", enETag)
	check("zh If-None-Match with en ETag -> 200", r8.code == 200, r8.code)
}

func phaseCoalescing() {
	fmt.Println("== concurrent request coalescing ==")
	must(resetOrigin())
	must(postConfig(map[string]any{
		"maxAge": 30, "mode": "ok", "version": 1,
		"vary": "accept-language", "delayMs": 300, "noETag": false,
	}))

	const workers = 20
	codes := make([]int, workers)
	bodies := make([]string, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := getAsset(proxyBase, "/assets/conc", "en", "")
			if err == nil {
				codes[i], bodies[i] = r.code, r.body
			}
		}(i)
	}
	close(start)
	wg.Wait()

	ok := bodies[0] != ""
	for i := range codes {
		if codes[i] != 200 || bodies[i] != bodies[0] {
			ok = false
		}
	}
	check("all 20 concurrent requests got the same 200 body", ok, codes)
	s, err := getStats()
	check("origin was hit exactly once", err == nil && s.Paths["/assets/conc"] == 1,
		s.Paths["/assets/conc"], err)

	must(postConfig(map[string]any{"delayMs": 0}))
}

func phaseRevalidation() {
	fmt.Println("== revalidation: 304 extends freshness ==")
	must(resetOrigin())
	must(postConfig(map[string]any{
		"maxAge": 1, "mode": "ok", "version": 1,
		"vary": "accept-language", "delayMs": 0, "noETag": false,
	}))

	r1, _ := getAsset(proxyBase, "/assets/rev", "en", "")
	check("initial request -> MISS", r1.code == 200 && r1.hdr.Get("X-Proxy-Cache") == "MISS",
		r1.code, r1.hdr.Get("X-Proxy-Cache"))

	time.Sleep(1300 * time.Millisecond) // entry expired (max-age=1)
	r2, _ := getAsset(proxyBase, "/assets/rev", "en", "")
	check("expired entry -> conditional revalidation (REVALIDATED)",
		r2.code == 200 && r2.hdr.Get("X-Proxy-Cache") == "REVALIDATED",
		r2.code, r2.hdr.Get("X-Proxy-Cache"))

	r3, _ := getAsset(proxyBase, "/assets/rev", "en", "")
	check("fresh again after 304 -> HIT without origin call",
		r3.code == 200 && r3.hdr.Get("X-Proxy-Cache") == "HIT", r3.code, r3.hdr.Get("X-Proxy-Cache"))

	s, err := getStats()
	check("origin saw 2 requests, exactly 1 conditional",
		err == nil && s.Paths["/assets/rev"] == 2 && s.ConditionalHits == 1,
		s.Paths["/assets/rev"], s.ConditionalHits, err)

	must(postConfig(map[string]any{"version": 2})) // content changes -> new ETag
	time.Sleep(1300 * time.Millisecond)
	r4, _ := getAsset(proxyBase, "/assets/rev", "en", "")
	check("changed content fetched with new version",
		r4.code == 200 && strings.Contains(r4.body, `"version":2`), r4.code, r4.body)
}

func phaseStale() {
	fmt.Println("== stale-if-error ==")
	must(resetOrigin())
	must(postConfig(map[string]any{
		"maxAge": 1, "mode": "ok", "version": 1,
		"vary": "accept-language", "delayMs": 0, "noETag": false,
	}))

	r1, _ := getAsset(proxyBase, "/assets/stale", "en", "")
	check("initial request cached", r1.code == 200 && r1.hdr.Get("X-Proxy-Cache") == "MISS",
		r1.code, r1.hdr.Get("X-Proxy-Cache"))

	time.Sleep(1300 * time.Millisecond) // expired
	must(postConfig(map[string]any{"mode": "error"}))
	r2, err := getAsset(proxyBase, "/assets/stale", "en", "")
	check("origin error -> stale 200 served",
		err == nil && r2.code == 200 && r2.hdr.Get("X-Proxy-Cache") == "STALE",
		r2.code, r2.hdr.Get("X-Proxy-Cache"), err)
	check("stale response clearly marked (Warning 110 + X-Proxy-Stale)",
		r2.hdr.Get("X-Proxy-Stale") == "true" && strings.Contains(r2.hdr.Get("Warning"), "110"),
		r2.hdr.Get("X-Proxy-Stale"), r2.hdr.Get("Warning"))
	check("stale body preserved", strings.Contains(r2.body, `"lang":"en"`), r2.body)

	// Proxy runs with STALE_IF_ERROR=3s: entry is servable until
	// storedAt + maxAge(1s) + 3s. Sleeping past that must yield 502.
	time.Sleep(3500 * time.Millisecond)
	r3, _ := getAsset(proxyBase, "/assets/stale", "en", "")
	check("beyond stale window -> 502", r3.code == http.StatusBadGateway, r3.code)

	must(postConfig(map[string]any{"mode": "ok"}))
	r4, _ := getAsset(proxyBase, "/assets/stale", "en", "")
	check("origin recovery -> fresh 200 without stale marker",
		r4.code == 200 && r4.hdr.Get("X-Proxy-Stale") == "", r4.code, r4.hdr.Get("X-Proxy-Stale"))
}

func phaseNonCacheable() {
	fmt.Println("== cacheability rules ==")
	must(resetOrigin())
	base := map[string]any{"mode": "ok", "version": 1, "delayMs": 0, "noETag": false}

	must(postConfig(with(base, "maxAge", 70, "vary", "accept-language")))
	checkPair("max-age>60 not cached", "/assets/nc1")

	must(postConfig(with(base, "maxAge", 30, "vary", "accept-encoding")))
	checkPair("Vary: Accept-Encoding not cached", "/assets/nc2")

	must(postConfig(with(base, "maxAge", 30, "vary", "both")))
	checkPair("Vary: Accept-Language, Accept-Encoding not cached", "/assets/nc3")

	must(postConfig(with(base, "maxAge", 30, "vary", "accept-language", "noETag", true)))
	checkPair("missing ETag not cached", "/assets/nc4")
	must(postConfig(map[string]any{"noETag": false}))
}

func with(base map[string]any, kvs ...any) map[string]any {
	m := make(map[string]any, len(base)+len(kvs)/2)
	for k, v := range base {
		m[k] = v
	}
	for i := 0; i+1 < len(kvs); i += 2 {
		m[kvs[i].(string)] = kvs[i+1]
	}
	return m
}

// checkPair issues two requests and expects both to pass through to the
// origin (nothing cached).
func checkPair(name, path string) {
	r1, e1 := getAsset(proxyBase, path, "en", "")
	r2, e2 := getAsset(proxyBase, path, "en", "")
	s, err := getStats()
	check(name,
		e1 == nil && e2 == nil && err == nil &&
			r1.code == 200 && r2.code == 200 &&
			r2.hdr.Get("X-Proxy-Cache") == "PASS" && s.Paths[path] == 2,
		r1.code, r2.code, r2.hdr.Get("X-Proxy-Cache"), s.Paths[path], e1, e2, err)
}

func phaseUnsupportedLanguage() {
	fmt.Println("== unsupported language bypasses cache ==")
	must(resetOrigin())
	must(postConfig(map[string]any{
		"maxAge": 30, "mode": "ok", "version": 1,
		"vary": "accept-language", "delayMs": 0, "noETag": false,
	}))
	r1, e1 := getAsset(proxyBase, "/assets/fr", "fr", "")
	r2, e2 := getAsset(proxyBase, "/assets/fr", "fr", "")
	s, err := getStats()
	check("fr requests are proxied but never cached",
		e1 == nil && e2 == nil && err == nil &&
			r1.code == 200 && r2.code == 200 &&
			r2.hdr.Get("X-Proxy-Cache") == "PASS" && s.Paths["/assets/fr"] == 2,
		r1.code, r2.code, r2.hdr.Get("X-Proxy-Cache"), s.Paths["/assets/fr"], e1, e2, err)
}
