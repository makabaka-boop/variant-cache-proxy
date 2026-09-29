// Command proxy runs the caching asset proxy.
//
// Environment:
//
//	PORT            listen port (default 8080)
//	ORIGIN_URL      origin base URL (default http://localhost:9000)
//	STALE_IF_ERROR  how long past expiry stale content may be served on
//	                origin error (default 30s)
//	FETCH_TIMEOUT   per-fetch origin timeout (default 10s)
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"assetproxy/internal/proxy"
)

func main() {
	port := getenv("PORT", "8080")
	origin := getenv("ORIGIN_URL", "http://localhost:9000")
	staleWindow := getduration("STALE_IF_ERROR", 30*time.Second)
	fetchTimeout := getduration("FETCH_TIMEOUT", 10*time.Second)

	p := proxy.New(origin, nil, nil, staleWindow, fetchTimeout)
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           p,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("proxy listening on :%s origin=%s stale-if-error=%s fetch-timeout=%s",
		port, origin, staleWindow, fetchTimeout)
	log.Fatal(srv.ListenAndServe())
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getduration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("invalid %s=%q, using default %s", key, v, def)
	}
	return def
}
