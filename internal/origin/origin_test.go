package origin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func do(t *testing.T, h http.Handler, method, path, lang, inm string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	if inm != "" {
		req.Header.Set("If-None-Match", inm)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAssetVariantsAndConditional(t *testing.T) {
	h := New().Handler()

	rec := do(t, h, http.MethodGet, "/assets/1", "en", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"lang":"en"`) {
		t.Fatalf("en: code=%d body=%q", rec.Code, rec.Body.String())
	}
	enETag := rec.Header().Get("ETag")
	if enETag == "" || rec.Header().Get("Cache-Control") == "" {
		t.Fatalf("missing ETag/Cache-Control: %v", rec.Header())
	}

	rec = do(t, h, http.MethodGet, "/assets/1", "zh-CN,zh;q=0.9", "")
	if !strings.Contains(rec.Body.String(), `"lang":"zh"`) {
		t.Fatalf("zh: body=%q", rec.Body.String())
	}
	zhETag := rec.Header().Get("ETag")
	if zhETag == enETag {
		t.Fatalf("variants must have distinct ETags, both %q", enETag)
	}

	rec = do(t, h, http.MethodGet, "/assets/1", "en", enETag)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("matching If-None-Match: code=%d, want 304", rec.Code)
	}
	rec = do(t, h, http.MethodGet, "/assets/1", "en", zhETag)
	if rec.Code != 200 {
		t.Fatalf("mismatched If-None-Match: code=%d, want 200", rec.Code)
	}
}

func TestControlEndpoints(t *testing.T) {
	h := New().Handler()

	rec := do(t, h, http.MethodPost, "/control/config", "", "")
	if rec.Code != 200 {
		t.Fatalf("empty config update: %d", rec.Code)
	}

	body := `{"mode":"error","maxAge":7,"version":3,"delayMs":0,"vary":"accept-encoding","noETag":true}`
	req := httptest.NewRequest(http.MethodPost, "/control/config", strings.NewReader(body))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("config update: %d", rec.Code)
	}

	rec = do(t, h, http.MethodGet, "/assets/1", "en", "")
	if rec.Code != 500 {
		t.Fatalf("error mode: code=%d, want 500", rec.Code)
	}

	do(t, h, http.MethodPost, "/control/reset", "", "")
	rec = do(t, h, http.MethodGet, "/assets/1", "en", "")
	if rec.Code != 200 {
		t.Fatalf("after reset: code=%d, want 200", rec.Code)
	}

	rec = do(t, h, http.MethodGet, "/control/stats", "", "")
	var s Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("stats decode: %v", err)
	}
	if s.Hits != 1 || s.Paths["/assets/1"] != 1 {
		t.Fatalf("stats after reset = %+v, want 1 hit on /assets/1", s)
	}
}

func TestStatsCountConditionals(t *testing.T) {
	h := New().Handler()
	do(t, h, http.MethodGet, "/assets/9", "en", "")
	do(t, h, http.MethodGet, "/assets/9", "en", `"whatever"`)
	rec := do(t, h, http.MethodGet, "/control/stats", "", "")
	var s Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("stats decode: %v", err)
	}
	if s.Hits != 2 || s.ConditionalHits != 1 {
		t.Fatalf("stats = %+v, want 2 hits / 1 conditional", s)
	}
}
