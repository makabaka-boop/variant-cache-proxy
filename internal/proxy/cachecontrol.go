package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	minCacheableMaxAge = 0
	maxCacheableMaxAge = 60
)

// parseCacheControl parses a Cache-Control header into a directive map.
// Directive names are lower-cased; values are unquoted.
func parseCacheControl(v string) map[string]string {
	d := make(map[string]string)
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, val, hasEq := strings.Cut(part, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		if hasEq {
			d[k] = strings.Trim(strings.TrimSpace(val), `"`)
		} else {
			d[k] = ""
		}
	}
	return d
}

// maxAgeDirective extracts a usable max-age from h: it must be present,
// parse as a non-negative integer, and fall within [0, 60] seconds.
// no-store / no-cache make the response uncacheable.
func maxAgeDirective(h http.Header) (time.Duration, bool) {
	cc := parseCacheControl(h.Get("Cache-Control"))
	if _, ok := cc["no-store"]; ok {
		return 0, false
	}
	if _, ok := cc["no-cache"]; ok {
		return 0, false
	}
	raw, ok := cc["max-age"]
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < minCacheableMaxAge || n > maxCacheableMaxAge {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

// varyAcceptLanguageOnly reports whether every Vary token is
// Accept-Language. Vary: * or any other field name is rejected.
func varyAcceptLanguageOnly(h http.Header) bool {
	for _, line := range h.Values("Vary") {
		for _, tok := range strings.Split(line, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			if !strings.EqualFold(tok, "Accept-Language") {
				return false
			}
		}
	}
	return true
}

// cacheableMaxAge reports whether a response may be stored: status 200,
// an ETag, max-age within [0,60], and Vary limited to Accept-Language.
func cacheableMaxAge(status int, h http.Header) (time.Duration, bool) {
	if status != http.StatusOK {
		return 0, false
	}
	if h.Get("ETag") == "" {
		return 0, false
	}
	ma, ok := maxAgeDirective(h)
	if !ok {
		return 0, false
	}
	if !varyAcceptLanguageOnly(h) {
		return 0, false
	}
	return ma, true
}
