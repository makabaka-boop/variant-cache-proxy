// Package origin implements a controllable origin server for the asset
// proxy: GET /assets/{id} plus /control/* endpoints to shape its behavior
// (max-age, failure mode, latency, version, Vary, ETag) at runtime.
package origin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Config controls how the origin answers asset requests.
type Config struct {
	MaxAge  int    `json:"maxAge"`  // Cache-Control max-age in seconds
	Mode    string `json:"mode"`    // "ok" or "error"
	DelayMs int    `json:"delayMs"` // artificial latency before responding
	Version int    `json:"version"` // bump to change the ETag and body
	Vary    string `json:"vary"`    // accept-language|accept-encoding|both|star|none
	NoETag  bool   `json:"noETag"`  // omit the ETag header
}

// DefaultConfig is restored by /control/reset.
func DefaultConfig() Config {
	return Config{MaxAge: 2, Mode: "ok", Version: 1, Vary: "accept-language"}
}

// Stats reports what the origin has seen since the last reset.
type Stats struct {
	Hits            int            `json:"hits"`
	ConditionalHits int            `json:"conditionalHits"`
	Paths           map[string]int `json:"paths"`
}

// Server is the controllable origin.
type Server struct {
	mu    sync.Mutex
	cfg   Config
	stats Stats
}

// New returns a Server with the default config.
func New() *Server {
	return &Server{cfg: DefaultConfig(), stats: Stats{Paths: map[string]int{}}}
}

// Handler returns the origin's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/assets/", s.handleAsset)
	mux.HandleFunc("/control/config", s.handleConfig)
	mux.HandleFunc("/control/stats", s.handleStats)
	mux.HandleFunc("/control/reset", s.handleReset)
	return mux
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/assets/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}

	s.mu.Lock()
	cfg := s.cfg
	s.stats.Hits++
	s.stats.Paths[r.URL.Path]++
	conditional := r.Header.Get("If-None-Match") != ""
	if conditional {
		s.stats.ConditionalHits++
	}
	s.mu.Unlock()

	if cfg.DelayMs > 0 {
		time.Sleep(time.Duration(cfg.DelayMs) * time.Millisecond)
	}
	if cfg.Mode == "error" {
		http.Error(w, "origin failure", http.StatusInternalServerError)
		return
	}

	lang := "en"
	if strings.HasPrefix(primaryLanguage(r.Header.Get("Accept-Language")), "zh") {
		lang = "zh"
	}
	etag := fmt.Sprintf("\"asset-%s-%s-v%d\"", id, lang, cfg.Version)

	h := w.Header()
	h.Set("Cache-Control", fmt.Sprintf("max-age=%d", cfg.MaxAge))
	switch cfg.Vary {
	case "accept-language":
		h.Set("Vary", "Accept-Language")
	case "accept-encoding":
		h.Set("Vary", "Accept-Encoding")
	case "both":
		h.Set("Vary", "Accept-Language, Accept-Encoding")
	case "star":
		h.Set("Vary", "*")
	case "none":
	}
	if !cfg.NoETag {
		h.Set("ETag", etag)
	}
	h.Set("Content-Type", "application/json")

	if conditional && matchETag(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"id":%q,"lang":%q,"version":%d}`, id, lang, cfg.Version)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		cfg := s.cfg
		s.mu.Unlock()
		writeJSON(w, cfg)
	case http.MethodPost:
		// Decode over the current config so absent fields keep their values.
		s.mu.Lock()
		cfg := s.cfg
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil && err != io.EOF {
			s.mu.Unlock()
			http.Error(w, "bad config: "+err.Error(), http.StatusBadRequest)
			return
		}
		s.cfg = cfg
		s.mu.Unlock()
		writeJSON(w, cfg)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	paths := make(map[string]int, len(s.stats.Paths))
	for k, v := range s.stats.Paths {
		paths[k] = v
	}
	writeJSON(w, Stats{Hits: s.stats.Hits, ConditionalHits: s.stats.ConditionalHits, Paths: paths})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	s.cfg = DefaultConfig()
	s.stats = Stats{Paths: map[string]int{}}
	s.mu.Unlock()
	writeJSON(w, map[string]string{"status": "reset"})
}

func primaryLanguage(al string) string {
	if i := strings.Index(al, ","); i >= 0 {
		al = al[:i]
	}
	if i := strings.Index(al, ";"); i >= 0 {
		al = al[:i]
	}
	return strings.ToLower(strings.TrimSpace(al))
}

func matchETag(inm, etag string) bool {
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
