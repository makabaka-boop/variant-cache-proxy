package proxy

import (
	"strconv"
	"strings"
)

// normalizeVariant maps an Accept-Language header to a supported cache
// variant. Only zh and en are supported; the highest-q language range wins.
// Anything else (including an empty header) yields ok=false, and callers
// must bypass the cache for such requests.
func normalizeVariant(header string) (variant string, ok bool) {
	best := ""
	bestQ := -1.0
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tag, q := part, 1.0
		if i := strings.Index(part, ";"); i >= 0 {
			tag = strings.TrimSpace(part[:i])
			for _, p := range strings.Split(part[i+1:], ";") {
				p = strings.TrimSpace(p)
				if v, found := strings.CutPrefix(p, "q="); found {
					if f, err := strconv.ParseFloat(v, 64); err == nil {
						q = f
					}
				}
			}
		}
		if q <= 0 {
			continue
		}
		if q > bestQ {
			bestQ = q
			best = tag
		}
	}
	primary := strings.ToLower(best)
	switch {
	case strings.HasPrefix(primary, "zh"):
		return "zh", true
	case strings.HasPrefix(primary, "en"):
		return "en", true
	default:
		return "", false
	}
}
